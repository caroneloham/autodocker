package handlers

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"fmt"
	"hash"
	"image/png"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"autodocker-go/config"
	"autodocker-go/db"
	"autodocker-go/middleware"
	"autodocker-go/services"

	xscrypt "golang.org/x/crypto/scrypt"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
	"github.com/pquerna/otp/totp"
	"github.com/skip2/go-qrcode"
	"golang.org/x/crypto/bcrypt"
)

// RoleLevel returns the numeric level for a role
func RoleLevel(r string) int {
	switch r {
	case "super_admin":
		return 3
	case "admin":
		return 2
	case "user":
		return 1
	default:
		return 0
	}
}

// ValidatePassword checks password strength (8+ chars, uppercase, lowercase, digit, special)
func ValidatePassword(password string) (bool, string) {
	if len(password) < 8 {
		return false, "Le mot de passe doit contenir au moins 8 caracteres"
	}
	if matched, _ := regexp.MatchString(`[A-Z]`, password); !matched {
		return false, "Le mot de passe doit contenir au moins une majuscule"
	}
	if matched, _ := regexp.MatchString(`[a-z]`, password); !matched {
		return false, "Le mot de passe doit contenir au moins une minuscule"
	}
	if matched, _ := regexp.MatchString(`[0-9]`, password); !matched {
		return false, "Le mot de passe doit contenir au moins un chiffre"
	}
	if matched, _ := regexp.MatchString(`[^A-Za-z0-9]`, password); !matched {
		return false, "Le mot de passe doit contenir au moins un caractere special"
	}
	return true, "OK"
}

// hashPassword creates a bcrypt hash
func hashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// checkPassword verifies a password against a hash (supports bcrypt AND werkzeug pbkdf2:sha256)
func checkPassword(hash, password string) bool {
	// Werkzeug format: pbkdf2:sha256:ITERATIONS$SALT$HASH
	if strings.HasPrefix(hash, "pbkdf2:sha256:") {
		return checkWerkzeugPassword(hash, password)
	}
	// Try scrypt format (werkzeug newer): scrypt:32768:8:1$SALT$HASH
	if strings.HasPrefix(hash, "scrypt:") {
		return checkWerkzeugPassword(hash, password)
	}
	// Standard bcrypt
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	return err == nil
}

// checkWerkzeugPassword verifies werkzeug's password hash formats:
// - pbkdf2:sha256:ITER$SALT$HASH
// - scrypt:N:R:P$SALT$HASH
func checkWerkzeugPassword(hashStr, password string) bool {
	parts := strings.SplitN(hashStr, "$", 3)
	if len(parts) != 3 {
		return false
	}
	methodParts := strings.Split(parts[0], ":")
	salt := parts[1]
	expectedHash := parts[2]

	if methodParts[0] == "scrypt" && len(methodParts) >= 4 {
		// scrypt:N:R:P$SALT$HASH
		var n, r, p int
		fmt.Sscanf(methodParts[1], "%d", &n)
		fmt.Sscanf(methodParts[2], "%d", &r)
		fmt.Sscanf(methodParts[3], "%d", &p)
		if n == 0 {
			n = 32768
		}
		if r == 0 {
			r = 8
		}
		if p == 0 {
			p = 1
		}
		dk, err := scryptKey([]byte(password), []byte(salt), n, r, p, 64)
		if err != nil {
			return false
		}
		computed := fmt.Sprintf("%x", dk)
		return computed == expectedHash
	}

	if methodParts[0] == "pbkdf2" && len(methodParts) >= 2 {
		hashMethod := methodParts[1]
		iterations := 600000
		if len(methodParts) >= 3 {
			fmt.Sscanf(methodParts[2], "%d", &iterations)
		}
		if hashMethod == "sha256" {
			dk := pbkdf2Key([]byte(password), []byte(salt), iterations, 32, sha256.New)
			computed := fmt.Sprintf("%x", dk)
			return computed == expectedHash
		}
	}

	return false
}

func scryptKey(password, salt []byte, N, r, p, keyLen int) ([]byte, error) {
	return xscrypt.Key(password, salt, N, r, p, keyLen)
}

func pbkdf2Key(password, salt []byte, iter, keyLen int, h func() hash.Hash) []byte {
	prf := hmac.New(h, password)
	numBlocks := (keyLen + prf.Size() - 1) / prf.Size()
	var dk []byte
	buf := make([]byte, 4)
	for block := 1; block <= numBlocks; block++ {
		buf[0] = byte(block >> 24)
		buf[1] = byte(block >> 16)
		buf[2] = byte(block >> 8)
		buf[3] = byte(block)
		prf.Reset()
		prf.Write(salt)
		prf.Write(buf)
		u := prf.Sum(nil)
		result := make([]byte, len(u))
		copy(result, u)
		for i := 1; i < iter; i++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(nil)
			for j := range result {
				result[j] ^= u[j]
			}
		}
		dk = append(dk, result...)
	}
	return dk[:keyLen]
}

// generateRandomPassword creates a secure random password
func generateRandomPassword(length int) string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!@#$%^&*"
	result := make([]byte, length)
	for i := range result {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		result[i] = charset[n.Int64()]
	}
	return string(result)
}

// EnsureAdminUser creates the admin user on first run if none exists.
// Security: no hardcoded credentials — a random password is generated once,
// written to secrets/.admin_password, and existing accounts are NEVER
// overwritten (so password changes survive restarts).
func EnsureAdminUser() {
	if config.AuthBypass {
		ensureAuthBypassUser()
	}
	if localBootstrapPending() {
		fmt.Println("[AUTH] Première ouverture locale : création du compte propriétaire dans le navigateur")
		return
	}

	const defaultUser = "admin"

	var count int
	err := db.DB.QueryRow("SELECT COUNT(*) FROM users WHERE role IN ('admin','super_admin')").Scan(&count)
	if err == nil && count > 0 {
		// An admin already exists — never touch existing credentials.
		return
	}

	// No admin at all — create one with a random password
	defaultPass := generateRandomPassword(16)
	hash, err := hashPassword(defaultPass)
	if err != nil {
		fmt.Printf("[AUTH] Erreur hash mot de passe admin: %v\n", err)
		return
	}

	_, err = db.DB.Exec(
		"INSERT OR IGNORE INTO users (username, password_hash, role) VALUES (?, ?, ?)",
		defaultUser, hash, "super_admin",
	)
	if err != nil {
		fmt.Printf("[AUTH] Erreur creation admin: %v\n", err)
		return
	}

	// Save password to secrets/.admin_password for reference
	pwFile := filepath.Join(config.SecretsDir, ".admin_password")
	os.WriteFile(pwFile, []byte(defaultPass), 0600)
	fmt.Printf("[AUTH] Admin '%s' cree. Mot de passe dans: %s\n", defaultUser, pwFile)
}

func ensureAuthBypassUser() {
	username := strings.TrimSpace(config.AuthBypassUser)
	if username == "" {
		username = "elohgam"
	}
	role := config.AuthBypassRole
	if role == "" {
		role = "super_admin"
	}

	pwHash, err := hashPassword(generateRandomPassword(16))
	if err != nil {
		fmt.Printf("[AUTH] Erreur hash utilisateur bypass: %v\n", err)
		return
	}

	_, err = db.DB.Exec(
		"INSERT OR IGNORE INTO users (username, password_hash, role, totp_enabled, totp_secret) VALUES (?, ?, ?, 0, '')",
		username, pwHash, role,
	)
	if err != nil {
		fmt.Printf("[AUTH] Erreur creation utilisateur bypass '%s': %v\n", username, err)
		return
	}

	_, err = db.DB.Exec(
		"UPDATE users SET role = ?, totp_enabled = 0, totp_secret = '' WHERE username = ?",
		role, username,
	)
	if err != nil {
		fmt.Printf("[AUTH] Erreur mise a jour utilisateur bypass '%s': %v\n", username, err)
		return
	}

	fmt.Printf("[AUTH] Bypass actif: '%s' connecte automatiquement (role=%s)\n", username, role)
}

// dummyHash is used for timing-attack prevention on non-existent users
var dummyHash string

func init() {
	h, _ := hashPassword("__dummy_timing_equalize__")
	dummyHash = h
}

// RegisterAuthRoutes registers all authentication routes
func RegisterAuthRoutes(r *gin.Engine, loginGroup *gin.RouterGroup, apiGroup *gin.RouterGroup) {
	// Public routes (login/logout)
	if config.AuthBypass {
		// Local bypass mode: no login form, the middleware injects the session.
		r.GET("/login", func(c *gin.Context) { c.Redirect(http.StatusFound, "/") })
		r.POST("/login", func(c *gin.Context) { c.Redirect(http.StatusFound, "/") })
		r.GET("/logout", func(c *gin.Context) { c.Redirect(http.StatusFound, "/") })
	} else if config.DemoMode {
		// Demo mode: redirect login to dashboard
		r.GET("/login", func(c *gin.Context) { c.Redirect(http.StatusFound, "/") })
		r.POST("/login", func(c *gin.Context) { c.Redirect(http.StatusFound, "/") })
		r.GET("/logout", func(c *gin.Context) { c.Redirect(http.StatusFound, "/") })
	} else {
		registerLocalBootstrapRoutes(r)
		r.GET("/login", handleLoginPage)
		r.POST("/login", middleware.RateLimitLogin(), handleLoginPost)
		r.GET("/logout", handleLogout)
	}

	// Authenticated API routes
	apiGroup.GET("/check-session", handleCheckSession)
	if config.GeneratorOnly {
		ownerTOTP := apiGroup.Group("/totp", middleware.AccountOwnerRequired())
		ownerTOTP.GET("/status", handleTOTPStatus)
		ownerTOTP.POST("/setup", handleTOTPSetup)
		ownerTOTP.POST("/verify", handleTOTPVerify)
		ownerTOTP.POST("/disable", handleTOTPDisable)
		return
	}
	apiGroup.POST("/change-password", handleChangePassword)

	// TOTP routes
	apiGroup.GET("/totp/status", handleTOTPStatus)
	apiGroup.POST("/totp/setup", handleTOTPSetup)
	apiGroup.POST("/totp/verify", handleTOTPVerify)
	apiGroup.POST("/totp/disable", handleTOTPDisable)
	apiGroup.POST("/admin/reset-totp", middleware.AdminRequired(), handleAdminResetTOTP)
}

// --- LOGIN ---

func handleLoginPage(c *gin.Context) {
	if localBootstrapPending() && localBootstrapRequest(c) {
		if bootstrapGrant(c) {
			c.Redirect(http.StatusSeeOther, "/setup")
		} else {
			c.String(http.StatusForbidden, "Premiere ouverture : utilisez le navigateur lance par AutoDocker.")
		}
		return
	}
	sess := sessions.Default(c)
	// Clear ALL session data on login page — prevents stale/corrupted cookies
	// from causing infinite redirect loops
	sess.Clear()
	sess.Save()
	c.HTML(http.StatusOK, "login.html", gin.H{"Error": "", "TOTPStep": false, "Username": ""})
}

func handleLoginPost(c *gin.Context) {
	if localBootstrapPending() && localBootstrapRequest(c) {
		if bootstrapGrant(c) {
			c.Redirect(http.StatusSeeOther, "/setup")
		} else {
			c.Status(http.StatusForbidden)
		}
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 8192)
	sess := sessions.Default(c)
	clientIP := c.ClientIP()

	// Check if this is the TOTP verification step
	if c.PostForm("totp_step") == "1" {
		handleTOTPLogin(c, sess, clientIP)
		return
	}

	// Normal username/password login
	username := strings.TrimSpace(c.PostForm("username"))
	password := c.PostForm("password")

	// Look up user
	var pwHash string
	var role string
	var totpSecret sql.NullString
	var totpEnabled, disabled, authVersion int
	err := db.DB.QueryRow(
		"SELECT password_hash, role, totp_secret, totp_enabled, disabled, auth_version FROM users WHERE username = ?",
		username,
	).Scan(&pwHash, &role, &totpSecret, &totpEnabled, &disabled, &authVersion)

	if err != nil {
		// User not found — check against dummy hash for timing-attack prevention
		checkPassword(dummyHash, password)
		middleware.RecordLoginAttempt(clientIP)
		fmt.Printf("[AUTH] Echec login: utilisateur '%s' introuvable (IP: %s)\n", username, clientIP)
		c.HTML(http.StatusOK, "login.html", gin.H{
			"Error": "Identifiants invalides.", "TOTPStep": false, "Username": "",
		})
		return
	}

	if !checkPassword(pwHash, password) || disabled != 0 {
		middleware.RecordLoginAttempt(clientIP)
		fmt.Printf("[AUTH] Echec login: mot de passe incorrect pour '%s' (IP: %s)\n", username, clientIP)
		c.HTML(http.StatusOK, "login.html", gin.H{
			"Error": "Identifiants invalides.", "TOTPStep": false, "Username": "",
		})
		return
	}

	// Password valid — check if TOTP is required
	secret := ""
	if totpSecret.Valid {
		secret = totpSecret.String
	}

	if middleware.IsAdminOrAbove(role) && totpEnabled == 1 && secret != "" {
		// Store pending user for TOTP step
		sess.Set("_totp_pending_user", username)
		sess.Set("_totp_pending_version", authVersion)
		sess.Save()
		c.HTML(http.StatusOK, "login.html", gin.H{
			"Error": "", "TOTPStep": true, "Username": username,
		})
		return
	}

	// Login directly — save session with verification
	sess.Clear() // clear any stale data from previous sessions
	sess.Set("user", username)
	sess.Set("auth_version", authVersion)
	sess.Set("role", role)
	if middleware.IsAdminOrAbove(role) && totpEnabled == 0 && !config.LocalBootstrapMode() {
		sess.Set("_totp_setup_required", true)
	}
	if err := sess.Save(); err != nil {
		fmt.Printf("[AUTH] ERREUR CRITIQUE sauvegarde session: %v\n", err)
		c.HTML(http.StatusInternalServerError, "login.html", gin.H{
			"Error":    "Erreur interne de session. Supprimez les cookies du site et reessayez.",
			"TOTPStep": false, "Username": "",
		})
		return
	}
	db.LogActivity(username, "login", "", fmt.Sprintf("role=%s", role))
	fmt.Printf("[AUTH] Login reussi: '%s' (role=%s, IP=%s)\n", username, role, clientIP)
	c.Redirect(http.StatusFound, "/")
}

func handleTOTPLogin(c *gin.Context, sess sessions.Session, clientIP string) {
	pendingUser, _ := sess.Get("_totp_pending_user").(string)
	if pendingUser == "" {
		c.HTML(http.StatusOK, "login.html", gin.H{
			"Error": "Session TOTP expiree. Veuillez recommencer.", "TOTPStep": false, "Username": "",
		})
		return
	}

	totpCode := strings.TrimSpace(c.PostForm("totp_code"))

	var role string
	var totpSecret sql.NullString
	var disabled, authVersion int
	err := db.DB.QueryRow(
		"SELECT role, totp_secret, disabled, auth_version FROM users WHERE username = ?", pendingUser,
	).Scan(&role, &totpSecret, &disabled, &authVersion)

	pendingVersion, _ := sess.Get("_totp_pending_version").(int)
	if err != nil || disabled != 0 || pendingVersion != authVersion || !totpSecret.Valid || totpSecret.String == "" {
		c.HTML(http.StatusOK, "login.html", gin.H{
			"Error": "Erreur de configuration TOTP.", "TOTPStep": true, "Username": pendingUser,
		})
		return
	}

	valid := totp.Validate(totpCode, totpSecret.String)
	if !valid {
		c.HTML(http.StatusOK, "login.html", gin.H{
			"Error": "Code TOTP invalide.", "TOTPStep": true, "Username": pendingUser,
		})
		return
	}

	// TOTP verified — complete login
	sess.Delete("_totp_pending_user")
	sess.Set("user", pendingUser)
	sess.Set("auth_version", authVersion)
	sess.Delete("_totp_pending_version")
	sess.Set("role", role)
	if err := sess.Save(); err != nil {
		fmt.Printf("[AUTH] ERREUR CRITIQUE sauvegarde session (TOTP): %v\n", err)
		c.HTML(http.StatusInternalServerError, "login.html", gin.H{
			"Error":    "Erreur interne de session. Supprimez les cookies du site et reessayez.",
			"TOTPStep": false, "Username": "",
		})
		return
	}
	db.LogActivity(pendingUser, "login", "", fmt.Sprintf("role=%s, totp=ok", role))
	c.Redirect(http.StatusFound, "/")
}

// --- LOGOUT ---

func handleLogout(c *gin.Context) {
	sess := sessions.Default(c)
	sess.Delete("user")
	sess.Delete("role")
	sess.Delete("_totp_pending_user")
	sess.Delete("_totp_setup_required")
	sess.Save()
	c.Redirect(http.StatusFound, "/login")
}

// --- CHECK SESSION ---

func handleCheckSession(c *gin.Context) {
	user, role := middleware.GetSessionUser(c)
	roleDisplay := role
	if config.DemoMode {
		roleDisplay = "demo"
	}
	c.JSON(http.StatusOK, gin.H{
		"status":           "ok",
		"user":             user,
		"role":             role,
		"role_display":     roleDisplay,
		"demo":             config.DemoMode,
		"docker_connected": services.EnsureDocker() != nil,
		"ai_available":     services.AIAvailable(),
	})
}

// --- CHANGE PASSWORD ---

func handleChangePassword(c *gin.Context) {
	var data struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := c.ShouldBindJSON(&data); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Donnees invalides"})
		return
	}

	if data.OldPassword == "" || data.NewPassword == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Ancien et nouveau mot de passe requis"})
		return
	}

	valid, msg := ValidatePassword(data.NewPassword)
	if !valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
		return
	}

	username, _ := middleware.GetSessionUser(c)

	var currentHash string
	var currentVersion int
	err := db.DB.QueryRow("SELECT password_hash, auth_version FROM users WHERE username = ?", username).Scan(&currentHash, &currentVersion)
	if err != nil || !checkPassword(currentHash, data.OldPassword) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Ancien mot de passe incorrect"})
		return
	}

	newHash, err := hashPassword(data.NewPassword)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Erreur interne"})
		return
	}

	result, err := db.DB.Exec("UPDATE users SET password_hash = ?, auth_version = auth_version + 1 WHERE username = ? AND password_hash = ? AND auth_version = ?", newHash, username, currentHash, currentVersion)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Erreur mise a jour"})
		return
	}
	updated, err := result.RowsAffected()
	if err != nil || updated != 1 {
		c.JSON(http.StatusConflict, gin.H{"error": "Compte modifie entre temps"})
		return
	}
	session := sessions.Default(c)
	session.Set("auth_version", currentVersion+1)
	if err := session.Save(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Erreur de session"})
		return
	}

	db.LogActivity(username, "change-password", "", "")
	c.JSON(http.StatusOK, gin.H{"status": "ok", "message": "Mot de passe modifie"})
}

// --- TOTP ---

func handleTOTPStatus(c *gin.Context) {
	if config.AuthBypass {
		c.JSON(http.StatusOK, gin.H{
			"enabled":        false,
			"required":       false,
			"setup_required": false,
			"auth_bypass":    true,
		})
		return
	}

	// Demo mode: TOTP visible but never enabled, never required to setup
	if config.DemoMode {
		c.JSON(http.StatusOK, gin.H{
			"enabled":        false,
			"required":       false,
			"setup_required": false,
			"demo":           true,
		})
		return
	}

	username, role := middleware.GetSessionUser(c)

	var totpEnabled int
	var totpSecret sql.NullString
	err := db.DB.QueryRow(
		"SELECT totp_enabled, totp_secret FROM users WHERE username = ?", username,
	).Scan(&totpEnabled, &totpSecret)

	enabled := false
	if err == nil && totpEnabled == 1 {
		enabled = true
	}

	sess := sessions.Default(c)
	setupRequired := false
	if v := sess.Get("_totp_setup_required"); v != nil && !config.LocalBootstrapMode() {
		setupRequired = true
	}

	c.JSON(http.StatusOK, gin.H{
		"enabled":        enabled,
		"required":       middleware.IsAdminOrAbove(role) && !config.LocalBootstrapMode(),
		"setup_required": setupRequired,
	})
}

func demoTOTPBlocked(c *gin.Context) bool {
	if config.AuthBypass {
		c.JSON(http.StatusForbidden, gin.H{
			"status":      "error",
			"error":       "TOTP indisponible pendant le bypass d'authentification",
			"auth_bypass": true,
		})
		return true
	}
	if config.DemoMode {
		c.JSON(http.StatusForbidden, gin.H{
			"status": "error",
			"error":  "TOTP indisponible en mode demonstration",
			"demo":   true,
		})
		return true
	}
	return false
}

func handleTOTPSetup(c *gin.Context) {
	if demoTOTPBlocked(c) {
		return
	}
	username, _ := middleware.GetSessionUser(c)

	// Generate TOTP secret
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      "AutoDocker",
		AccountName: username,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Erreur generation TOTP"})
		return
	}

	secret := key.Secret()
	uri := key.URL()

	// Generate QR code as base64 PNG
	qrPNG, err := qrcode.Encode(uri, qrcode.Medium, 256)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Erreur generation QR code"})
		return
	}

	// Verify the PNG is valid by decoding it
	_, err = png.Decode(bytes.NewReader(qrPNG))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Erreur generation QR code"})
		return
	}

	qrB64 := base64.StdEncoding.EncodeToString(qrPNG)

	// Store secret in DB
	_, err = db.DB.Exec("UPDATE users SET totp_secret = ? WHERE username = ?", secret, username)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Erreur sauvegarde secret"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status": "ok",
		"qr":     qrB64,
		"secret": secret,
		"uri":    uri,
	})
}

func handleTOTPVerify(c *gin.Context) {
	if demoTOTPBlocked(c) {
		return
	}
	username, _ := middleware.GetSessionUser(c)

	var data struct {
		Code string `json:"code"`
	}
	if err := c.ShouldBindJSON(&data); err != nil {
		data.Code = ""
	}
	code := strings.TrimSpace(data.Code)

	var totpSecret sql.NullString
	err := db.DB.QueryRow("SELECT totp_secret FROM users WHERE username = ?", username).Scan(&totpSecret)
	if err != nil || !totpSecret.Valid || totpSecret.String == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "TOTP non configure. Lancez le setup d'abord."})
		return
	}

	if !totp.Validate(code, totpSecret.String) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Code TOTP invalide"})
		return
	}

	_, err = db.DB.Exec("UPDATE users SET totp_enabled = 1 WHERE username = ?", username)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Erreur activation TOTP"})
		return
	}

	sess := sessions.Default(c)
	sess.Delete("_totp_setup_required")
	sess.Save()

	db.LogActivity(username, "totp-enable", "", "TOTP active")
	c.JSON(http.StatusOK, gin.H{"status": "ok", "message": "TOTP active avec succes"})
}

func handleTOTPDisable(c *gin.Context) {
	if demoTOTPBlocked(c) {
		return
	}
	username, role := middleware.GetSessionUser(c)

	if middleware.IsAdminOrAbove(role) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Les admins ne peuvent pas desactiver le TOTP"})
		return
	}

	_, err := db.DB.Exec(
		"UPDATE users SET totp_enabled = 0, totp_secret = NULL WHERE username = ?", username,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Erreur desactivation TOTP"})
		return
	}

	db.LogActivity(username, "totp-disable", "", "TOTP desactive")
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func handleAdminResetTOTP(c *gin.Context) {
	if demoTOTPBlocked(c) {
		return
	}
	_, role := middleware.GetSessionUser(c)

	if role != "super_admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Seul un super_admin peut reset le TOTP"})
		return
	}

	var data struct {
		Username string `json:"username"`
	}
	if err := c.ShouldBindJSON(&data); err != nil || data.Username == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Username requis"})
		return
	}

	_, err := db.DB.Exec(
		"UPDATE users SET totp_enabled = 0, totp_secret = NULL WHERE username = ?", data.Username,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Erreur reset TOTP"})
		return
	}

	user, _ := middleware.GetSessionUser(c)
	db.LogActivity(user, "totp-reset", data.Username, "Reset TOTP par super_admin")
	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"message": fmt.Sprintf("TOTP reinitialise pour %s", data.Username),
	})
}
