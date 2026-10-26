package handlers

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/mail"
	"regexp"
	"strings"

	"autodocker-go/db"
	"autodocker-go/middleware"
	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
)

func RegisterAccountRoutes(r *gin.Engine, api *gin.RouterGroup) {
	r.GET("/compte", middleware.LoginRequired(), func(c *gin.Context) { accountPage(c, false) })
	r.GET("/utilisateurs", middleware.LoginRequired(), middleware.AccountOwnerRequired(), func(c *gin.Context) { accountPage(c, true) })
	api.GET("/account", handleAccountGet)
	api.POST("/account/email", handleAccountEmail)
	api.POST("/account/password", middleware.RateLimitLogin(), handleAccountPassword)
	registerLocalSetupRoutes(api)
	owner := api.Group("/accounts", middleware.AccountOwnerRequired())
	owner.GET("", handleAccountsList)
	owner.POST("", handleAccountCreate)
	owner.POST("/:username/status", handleAccountStatus)
	owner.POST("/:username/password", handleAccountResetPassword)
}

func accountPage(c *gin.Context, management bool) {
	c.Header("Cache-Control", "no-store")
	c.HTML(200, "accounts.html", gin.H{"CSRF": middleware.GenerateCSRFToken(c), "Management": management})
}

func decodeAccountRequest(c *gin.Context, target interface{}) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		c.JSON(400, gin.H{"error": "Demande invalide."})
		return false
	}
	if err := decoder.Decode(new(interface{})); err != io.EOF {
		c.JSON(400, gin.H{"error": "Demande invalide."})
		return false
	}
	c.Header("Cache-Control", "no-store")
	return true
}

func handleAccountGet(c *gin.Context) {
	user, _ := middleware.GetSessionUser(c)
	var email string
	if err := db.DB.QueryRow("SELECT email FROM users WHERE username=?", user).Scan(&email); err != nil {
		c.JSON(503, gin.H{"error": "Compte indisponible."})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(200, gin.H{"username": user, "email": email, "manage_users": middleware.IsAccountOwner(c)})
}

func handleAccountEmail(c *gin.Context) {
	var data struct {
		Email string `json:"email"`
	}
	if !decodeAccountRequest(c, &data) {
		return
	}
	email := strings.TrimSpace(data.Email)
	if email != "" {
		parsed, err := mail.ParseAddress(email)
		if err != nil || parsed.Address != email || len(email) > 254 || strings.ContainsAny(email, "\r\n") {
			c.JSON(400, gin.H{"error": "Indiquez une adresse e-mail valide."})
			return
		}
	}
	user, _ := middleware.GetSessionUser(c)
	if _, err := db.DB.Exec("UPDATE users SET email=? WHERE username=?", email, user); err != nil {
		c.JSON(503, gin.H{"error": "L’adresse n’a pas pu être enregistrée."})
		return
	}
	c.JSON(200, gin.H{"status": "ok", "message": "Adresse e-mail enregistrée."})
}

func validAccountPassword(c *gin.Context, password string) bool {
	if len(password) > 72 {
		c.JSON(400, gin.H{"error": "Le mot de passe doit faire au maximum 72 octets."})
		return false
	}
	if ok, message := ValidatePassword(password); !ok {
		c.JSON(400, gin.H{"error": message})
		return false
	}
	return true
}

func handleAccountPassword(c *gin.Context) {
	var data struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if !decodeAccountRequest(c, &data) || !validAccountPassword(c, data.New) {
		return
	}
	user, _ := middleware.GetSessionUser(c)
	var current string
	var version int
	if err := db.DB.QueryRow("SELECT password_hash,auth_version FROM users WHERE username=?", user).Scan(&current, &version); err != nil || !checkPassword(current, data.Current) {
		middleware.RecordLoginAttempt(c.ClientIP())
		c.JSON(403, gin.H{"error": "Mot de passe actuel incorrect."})
		return
	}
	hash, err := hashPassword(data.New)
	if err != nil {
		c.JSON(503, gin.H{"error": "Modification indisponible."})
		return
	}
	result, err := db.DB.Exec("UPDATE users SET password_hash=?,auth_version=auth_version+1 WHERE username=? AND password_hash=?", hash, user, current)
	if err != nil {
		c.JSON(503, gin.H{"error": "Modification indisponible."})
		return
	}
	if count, _ := result.RowsAffected(); count != 1 {
		c.JSON(409, gin.H{"error": "Le compte a changé. Reconnectez-vous."})
		return
	}
	session := sessions.Default(c)
	session.Set("auth_version", version+1)
	if err := session.Save(); err != nil {
		c.JSON(503, gin.H{"error": "Mot de passe modifié. Reconnectez-vous."})
		return
	}
	db.LogActivity(user, "change-password", "", "")
	c.JSON(200, gin.H{"status": "ok", "message": "Mot de passe modifié. Vos autres sessions ont été fermées."})
}

type managedAccount struct {
	Username string `json:"username"`
	Email    string `json:"email"`
	Disabled bool   `json:"disabled"`
	Owner    bool   `json:"owner"`
}

func handleAccountsList(c *gin.Context) {
	rows, err := db.DB.Query("SELECT username,email,disabled FROM users ORDER BY lower(username)")
	if err != nil {
		c.JSON(503, gin.H{"error": "Liste indisponible."})
		return
	}
	defer rows.Close()
	users := []managedAccount{}
	for rows.Next() {
		var user managedAccount
		var disabled int
		if err := rows.Scan(&user.Username, &user.Email, &disabled); err != nil {
			c.JSON(503, gin.H{"error": "Liste indisponible."})
			return
		}
		user.Disabled = disabled != 0
		user.Owner = user.Username == middleware.AccountOwnerUsername()
		users = append(users, user)
	}
	if rows.Err() != nil {
		c.JSON(503, gin.H{"error": "Liste indisponible."})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(200, gin.H{"users": users})
}

func handleAccountCreate(c *gin.Context) {
	var data struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeAccountRequest(c, &data) {
		return
	}
	username := strings.ToLower(strings.TrimSpace(data.Username))
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{2,31}$`).MatchString(username) {
		c.JSON(400, gin.H{"error": "Identifiant : 3 à 32 lettres minuscules, chiffres, points ou tirets."})
		return
	}
	if !validAccountPassword(c, data.Password) {
		return
	}
	var exists int
	if err := db.DB.QueryRow("SELECT count(*) FROM users WHERE username=? COLLATE NOCASE", username).Scan(&exists); err != nil {
		c.JSON(503, gin.H{"error": "Création indisponible."})
		return
	}
	if exists > 0 {
		c.JSON(409, gin.H{"error": "Cet identifiant existe déjà."})
		return
	}
	hash, err := hashPassword(data.Password)
	if err != nil {
		c.JSON(503, gin.H{"error": "Création indisponible."})
		return
	}
	if _, err := db.DB.Exec("INSERT INTO users(username,password_hash,role) VALUES(?,?,'user')", username, hash); err != nil {
		c.JSON(409, gin.H{"error": "Ce compte n’a pas pu être créé. Vérifiez l’identifiant."})
		return
	}
	owner, _ := middleware.GetSessionUser(c)
	db.LogActivity(owner, "create-user", username, "role=user")
	c.JSON(http.StatusCreated, gin.H{"status": "ok", "username": username})
}

func managedAccountTarget(c *gin.Context) (string, bool) {
	username := c.Param("username")
	if username == middleware.AccountOwnerUsername() {
		c.JSON(403, gin.H{"error": "Votre compte propriétaire se modifie depuis Mon compte."})
		return "", false
	}
	var exists int
	if err := db.DB.QueryRow("SELECT 1 FROM users WHERE username=?", username).Scan(&exists); err == sql.ErrNoRows {
		c.JSON(404, gin.H{"error": "Compte introuvable."})
		return "", false
	} else if err != nil {
		c.JSON(503, gin.H{"error": "Compte indisponible."})
		return "", false
	}
	return username, true
}

func handleAccountStatus(c *gin.Context) {
	var data struct {
		Disabled *bool `json:"disabled"`
	}
	if !decodeAccountRequest(c, &data) {
		return
	}
	if data.Disabled == nil {
		c.JSON(400, gin.H{"error": "État requis."})
		return
	}
	username, ok := managedAccountTarget(c)
	if !ok {
		return
	}
	if _, err := db.DB.Exec("UPDATE users SET disabled=?,auth_version=auth_version+1 WHERE username=?", *data.Disabled, username); err != nil {
		c.JSON(503, gin.H{"error": "Modification indisponible."})
		return
	}
	owner, _ := middleware.GetSessionUser(c)
	db.LogActivity(owner, "account-status", username, "")
	c.JSON(200, gin.H{"status": "ok"})
}

func handleAccountResetPassword(c *gin.Context) {
	var data struct {
		Password string `json:"password"`
	}
	if !decodeAccountRequest(c, &data) || !validAccountPassword(c, data.Password) {
		return
	}
	username, ok := managedAccountTarget(c)
	if !ok {
		return
	}
	hash, err := hashPassword(data.Password)
	if err != nil {
		c.JSON(503, gin.H{"error": "Réinitialisation indisponible."})
		return
	}
	if _, err := db.DB.Exec("UPDATE users SET password_hash=?,auth_version=auth_version+1 WHERE username=?", hash, username); err != nil {
		c.JSON(503, gin.H{"error": "Réinitialisation indisponible."})
		return
	}
	owner, _ := middleware.GetSessionUser(c)
	db.LogActivity(owner, "reset-password", username, "")
	c.JSON(200, gin.H{"status": "ok"})
}
