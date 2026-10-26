package handlers

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"autodocker-go/config"
	"autodocker-go/db"
	"autodocker-go/middleware"
	"autodocker-go/services"

	"github.com/docker/docker/api/types/container"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

// RegisterAdminRoutes registers all admin CRUD routes
func RegisterAdminRoutes(api *gin.RouterGroup) {
	admin := api.Group("/admin")
	admin.Use(middleware.AdminRequired())
	{
		admin.GET("/list-users", handleListUsers)
		admin.POST("/add-user", handleAddUser)
		admin.POST("/delete-user", handleDeleteUser)

		admin.POST("/create-group", handleCreateGroup)
		admin.POST("/delete-group", handleDeleteGroup)
		admin.POST("/add-to-group", handleAddToGroup)

		admin.GET("/user-permissions/:username", handleUserPermissions)
		admin.POST("/update-permission", handleUpdatePermission)

		admin.GET("/group-permissions/:group_name", handleGroupPermissions)
		admin.POST("/update-group-permission", handleUpdateGroupPermission)
	}

	// Activity log (admin required, but at /api/ level)
	api.GET("/activity-log", middleware.AdminRequired(), handleActivityLog)
}

// --- USERS ---

func handleListUsers(c *gin.Context) {
	rows, err := db.DB.Query("SELECT username, role, totp_enabled FROM users")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()

	type userInfo struct {
		Username    string `json:"username"`
		Role        string `json:"role"`
		TOTPEnabled bool   `json:"totp_enabled"`
	}
	var users []userInfo
	for rows.Next() {
		var u userInfo
		var totpInt int
		rows.Scan(&u.Username, &u.Role, &totpInt)
		u.TOTPEnabled = totpInt == 1
		users = append(users, u)
	}
	if users == nil {
		users = []userInfo{}
	}

	// Groups
	groupRows, _ := db.DB.Query("SELECT group_name FROM groups")
	var groups []string
	if groupRows != nil {
		defer groupRows.Close()
		for groupRows.Next() {
			var g string
			groupRows.Scan(&g)
			groups = append(groups, g)
		}
	}
	if groups == nil {
		groups = []string{}
	}

	// Membership
	memRows, _ := db.DB.Query("SELECT username, group_name FROM user_groups")
	type membership struct {
		User  string `json:"user"`
		Group string `json:"group"`
	}
	var memberships []membership
	if memRows != nil {
		defer memRows.Close()
		for memRows.Next() {
			var m membership
			memRows.Scan(&m.User, &m.Group)
			memberships = append(memberships, m)
		}
	}
	if memberships == nil {
		memberships = []membership{}
	}

	c.JSON(http.StatusOK, gin.H{
		"users":      users,
		"groups":     groups,
		"membership": memberships,
	})
}

func handleAddUser(c *gin.Context) {
	var data struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if err := c.ShouldBindJSON(&data); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "Donnees invalides"})
		return
	}
	if data.Role == "" {
		data.Role = "user"
	}

	if data.Username == "" || data.Password == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "Champs manquants"})
		return
	}

	// Validate username format
	matched, _ := regexp.MatchString(`^[a-zA-Z0-9_.\-]{1,32}$`, data.Username)
	if !matched {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": "error",
			"error":  "Nom d'utilisateur invalide (alphanum, _, ., - uniquement, max 32 chars)",
		})
		return
	}

	valid, msg := ValidatePassword(data.Password)
	if !valid {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": msg})
		return
	}

	_, myRole := middleware.GetSessionUser(c)

	if data.Role == "super_admin" && myRole != "super_admin" {
		c.JSON(http.StatusForbidden, gin.H{"status": "error", "error": "Seul un super_admin peut creer un super_admin"})
		return
	}
	if data.Role == "admin" && myRole != "super_admin" {
		c.JSON(http.StatusForbidden, gin.H{"status": "error", "error": "Seul un super_admin peut creer un admin"})
		return
	}

	// Check if user already exists
	var exists int
	db.DB.QueryRow("SELECT COUNT(*) FROM users WHERE username = ?", data.Username).Scan(&exists)
	if exists > 0 {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "Cet utilisateur existe deja"})
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(data.Password), bcrypt.DefaultCost)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": err.Error()})
		return
	}

	_, err = db.DB.Exec(
		"INSERT INTO users (username, password_hash, role) VALUES (?, ?, ?)",
		data.Username, string(hash), data.Role,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": err.Error()})
		return
	}

	user, _ := middleware.GetSessionUser(c)
	db.LogActivity(user, "create-user", data.Username, fmt.Sprintf("role=%s", data.Role))
	c.JSON(http.StatusOK, gin.H{"status": "ok", "message": fmt.Sprintf("Utilisateur %s cree", data.Username)})
}

func handleDeleteUser(c *gin.Context) {
	var data struct {
		Username string `json:"username"`
	}
	if err := c.ShouldBindJSON(&data); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Donnees invalides"})
		return
	}

	if data.Username == middleware.AccountOwnerUsername() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Impossible de supprimer l'admin racine"})
		return
	}

	_, myRole := middleware.GetSessionUser(c)

	var targetRole string
	err := db.DB.QueryRow("SELECT role FROM users WHERE username = ?", data.Username).Scan(&targetRole)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Utilisateur introuvable"})
		return
	}

	if RoleLevel(targetRole) >= RoleLevel(myRole) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Impossible de supprimer un utilisateur de rang egal ou superieur"})
		return
	}

	db.DB.Exec("DELETE FROM users WHERE username = ?", data.Username)
	db.DB.Exec("DELETE FROM user_groups WHERE username = ?", data.Username)

	user, _ := middleware.GetSessionUser(c)
	db.LogActivity(user, "delete-user", data.Username, "")
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// --- GROUPS ---

func handleCreateGroup(c *gin.Context) {
	var data struct {
		GroupName string `json:"group_name"`
	}
	if err := c.ShouldBindJSON(&data); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Donnees invalides"})
		return
	}

	groupName := strings.TrimSpace(data.GroupName)
	if groupName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Nom vide"})
		return
	}

	_, err := db.DB.Exec("INSERT INTO groups (group_name) VALUES (?)", groupName)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Le groupe existe deja"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func handleDeleteGroup(c *gin.Context) {
	var data struct {
		GroupName string `json:"group_name"`
	}
	if err := c.ShouldBindJSON(&data); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Donnees invalides"})
		return
	}

	db.DB.Exec("DELETE FROM groups WHERE group_name = ?", data.GroupName)
	db.DB.Exec("DELETE FROM user_groups WHERE group_name = ?", data.GroupName)
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func handleAddToGroup(c *gin.Context) {
	var data struct {
		Username  string `json:"username"`
		GroupName string `json:"group_name"`
	}
	if err := c.ShouldBindJSON(&data); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Donnees invalides"})
		return
	}

	db.DB.Exec("INSERT OR IGNORE INTO user_groups (username, group_name) VALUES (?, ?)",
		data.Username, data.GroupName)
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// --- PERMISSIONS ---

// getActiveProjects returns the set of active Docker Compose projects (lowercase)
func getActiveProjects() map[string]bool {
	active := make(map[string]bool)
	if services.EnsureDocker() == nil {
		return active
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	containers, err := services.EnsureDocker().ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return active
	}
	for _, ctr := range containers {
		if proj, ok := ctr.Labels["com.docker.compose.project"]; ok {
			active[strings.ToLower(proj)] = true
		}
	}
	return active
}

func handleUserPermissions(c *gin.Context) {
	targetUser := c.Param("username")

	activeProjects := getActiveProjects()

	// Check target user role
	var targetRole string
	err := db.DB.QueryRow("SELECT role FROM users WHERE username = ?", targetUser).Scan(&targetRole)
	isTargetAdmin := err == nil && middleware.IsAdminOrAbove(targetRole)

	// Get lab owners
	owners := make(map[string]string)
	ownerRows, _ := db.DB.Query("SELECT project, owner FROM labs")
	if ownerRows != nil {
		defer ownerRows.Close()
		for ownerRows.Next() {
			var proj, owner string
			ownerRows.Scan(&proj, &owner)
			owners[strings.ToLower(proj)] = owner
		}
	}

	// Get user ACLs
	userACLs := make(map[string]string)
	aclRows, _ := db.DB.Query("SELECT project, role FROM labs_acl WHERE username = ?", targetUser)
	if aclRows != nil {
		defer aclRows.Close()
		for aclRows.Next() {
			var proj, role string
			aclRows.Scan(&proj, &role)
			userACLs[strings.ToLower(proj)] = role
		}
	}

	type permEntry struct {
		Project string `json:"project"`
		Role    string `json:"role"`
	}
	var result []permEntry

	// Scan project directories
	if entries, err := os.ReadDir(config.ProjectsDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			pName := strings.ToLower(entry.Name())
			if !activeProjects[pName] {
				continue
			}
			currentRole := "none"
			if isTargetAdmin {
				currentRole = "write"
			} else if owners[pName] == targetUser {
				currentRole = "write"
			} else if acl, ok := userACLs[pName]; ok {
				currentRole = acl
			}
			result = append(result, permEntry{Project: entry.Name(), Role: currentRole})
		}
	}

	if result == nil {
		result = []permEntry{}
	}

	// Sort by project name (simple bubble sort for small lists)
	for i := 0; i < len(result); i++ {
		for j := i + 1; j < len(result); j++ {
			if result[i].Project > result[j].Project {
				result[i], result[j] = result[j], result[i]
			}
		}
	}

	c.JSON(http.StatusOK, result)
}

func handleUpdatePermission(c *gin.Context) {
	var data struct {
		Project  string `json:"project"`
		Username string `json:"username"`
		Role     string `json:"role"`
	}
	if err := c.ShouldBindJSON(&data); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Donnees invalides"})
		return
	}

	if data.Role == "none" {
		db.DB.Exec("DELETE FROM labs_acl WHERE project = ? AND username = ?",
			data.Project, data.Username)
	} else {
		db.DB.Exec("INSERT OR REPLACE INTO labs_acl (project, username, role) VALUES (?, ?, ?)",
			data.Project, data.Username, data.Role)
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func handleGroupPermissions(c *gin.Context) {
	groupName := c.Param("group_name")

	activeProjects := getActiveProjects()

	// Get group ACLs
	groupACLs := make(map[string]string)
	rows, _ := db.DB.Query("SELECT project, role FROM groups_acl WHERE group_name = ?", groupName)
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var proj, role string
			rows.Scan(&proj, &role)
			groupACLs[strings.ToLower(proj)] = role
		}
	}

	type permEntry struct {
		Project string `json:"project"`
		Role    string `json:"role"`
	}
	var result []permEntry

	if entries, err := os.ReadDir(config.ProjectsDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			pName := strings.ToLower(entry.Name())
			if !activeProjects[pName] {
				continue
			}
			role := "none"
			if acl, ok := groupACLs[pName]; ok {
				role = acl
			}
			result = append(result, permEntry{Project: entry.Name(), Role: role})
		}
	}

	if result == nil {
		result = []permEntry{}
	}

	// Sort by project name
	for i := 0; i < len(result); i++ {
		for j := i + 1; j < len(result); j++ {
			if result[i].Project > result[j].Project {
				result[i], result[j] = result[j], result[i]
			}
		}
	}

	c.JSON(http.StatusOK, result)
}

func handleUpdateGroupPermission(c *gin.Context) {
	var data struct {
		GroupName string `json:"group_name"`
		Project   string `json:"project"`
		Role      string `json:"role"`
	}
	if err := c.ShouldBindJSON(&data); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Donnees invalides"})
		return
	}

	if data.Role == "none" {
		db.DB.Exec("DELETE FROM groups_acl WHERE group_name = ? AND project = ?",
			data.GroupName, data.Project)
	} else {
		db.DB.Exec("INSERT OR REPLACE INTO groups_acl (group_name, project, role) VALUES (?, ?, ?)",
			data.GroupName, data.Project, data.Role)
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// --- ACTIVITY LOG ---

func handleActivityLog(c *gin.Context) {
	rows, err := db.DB.Query(
		"SELECT timestamp, username, action, target, details FROM activity_log ORDER BY timestamp DESC LIMIT 200",
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()

	type logEntry struct {
		Date     string `json:"date"`
		Username string `json:"username"`
		Action   string `json:"action"`
		Target   string `json:"target"`
		Details  string `json:"details"`
	}
	var logs []logEntry
	for rows.Next() {
		var l logEntry
		rows.Scan(&l.Date, &l.Username, &l.Action, &l.Target, &l.Details)
		logs = append(logs, l)
	}
	if logs == nil {
		logs = []logEntry{}
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "logs": logs})
}

// ListAllContainersHelper is a helper that wraps the services call for admin use
// It ensures we don't crash if Docker is unavailable
func listProjectDirs() []string {
	var dirs []string
	entries, err := os.ReadDir(filepath.Join(config.ProjectsDir))
	if err != nil {
		return dirs
	}
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	return dirs
}
