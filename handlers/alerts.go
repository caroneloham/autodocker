package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/smtp"
	"os"
	"path/filepath"
	"strings"
	"time"

	"autodocker-go/config"
	"autodocker-go/db"
	"autodocker-go/middleware"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
)

// AlertChannelConfig represents a single notification channel.
type AlertChannelConfig struct {
	Type    string                 `json:"type"`
	Enabled bool                   `json:"enabled"`
	Config  map[string]interface{} `json:"config"`
}

// AlertChannelsFile holds the full alert configuration.
type AlertChannelsFile struct {
	Channels []AlertChannelConfig `json:"channels"`
}

// AlertData is the payload sent to dispatchers.
type AlertData struct {
	RuleName   string   `json:"rule_name"`
	Severity   string   `json:"severity"`
	Message    string   `json:"message"`
	Timestamp  string   `json:"timestamp"`
	Containers []string `json:"containers"`
}

func alertChannelsFilePath() string {
	return filepath.Join(config.RootDir, "alert_channels.json")
}

func defaultAlertConfig() AlertChannelsFile {
	return AlertChannelsFile{
		Channels: []AlertChannelConfig{
			{Type: "email", Enabled: false, Config: map[string]interface{}{
				"smtp_host": "", "smtp_port": 587, "smtp_user": "", "smtp_pass": "",
				"from_addr": "", "to_addrs": []interface{}{},
			}},
			{Type: "discord", Enabled: false, Config: map[string]interface{}{
				"webhook_url": "",
			}},
			{Type: "webhook", Enabled: false, Config: map[string]interface{}{
				"url": "",
			}},
		},
	}
}

func loadAlertConfig() AlertChannelsFile {
	data, err := os.ReadFile(alertChannelsFilePath())
	if err != nil {
		return defaultAlertConfig()
	}
	var cfg AlertChannelsFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		return defaultAlertConfig()
	}
	return cfg
}

func saveAlertConfig(cfg AlertChannelsFile) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(alertChannelsFilePath(), data, 0644)
}

func getChannel(cfg AlertChannelsFile, channelType string) *AlertChannelConfig {
	for i := range cfg.Channels {
		if cfg.Channels[i].Type == channelType {
			return &cfg.Channels[i]
		}
	}
	return nil
}

// RegisterAlertRoutes registers all alert channel management routes.
func RegisterAlertRoutes(api *gin.RouterGroup) {
	g := api.Group("/alert-channels")
	g.Use(middleware.AdminRequired())
	{
		g.GET("", handleGetAlertChannels)
		g.POST("", handleSaveAlertChannels)
		g.POST("/test", handleTestAlertChannel)
	}
}

// ---------------------------------------------------------------------------
// Dispatchers
// ---------------------------------------------------------------------------

func dispatchWebhook(channel AlertChannelConfig, alert AlertData) gin.H {
	urlVal, _ := channel.Config["webhook_url"].(string)
	if urlVal == "" {
		urlVal, _ = channel.Config["url"].(string)
	}
	if urlVal == "" {
		return gin.H{"channel": "webhook", "success": false, "error": "URL webhook non configuree"}
	}

	payload, _ := json.Marshal(alert)
	resp, err := http.Post(urlVal, "application/json", bytes.NewReader(payload))
	if err != nil {
		return gin.H{"channel": "webhook", "success": false, "error": err.Error()}
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return gin.H{"channel": "webhook", "success": true, "error": nil}
	}
	return gin.H{"channel": "webhook", "success": false, "error": fmt.Sprintf("HTTP %d", resp.StatusCode)}
}

func dispatchDiscord(channel AlertChannelConfig, alert AlertData) gin.H {
	webhookURL, _ := channel.Config["webhook_url"].(string)
	if webhookURL == "" {
		return gin.H{"channel": "discord", "success": false, "error": "Configuration Discord incomplete"}
	}

	colorMap := map[string]int{"critical": 0xFF0000, "warning": 0xFF8C00, "info": 0x3498DB}
	color := colorMap[alert.Severity]
	if color == 0 {
		color = 0x3498DB
	}

	containersStr := "N/A"
	if len(alert.Containers) > 0 {
		containersStr = strings.Join(alert.Containers, ", ")
	}

	embed := map[string]interface{}{
		"title":       fmt.Sprintf("Alerte AutoDocker - %s", strings.ToUpper(alert.Severity)),
		"description": alert.Message,
		"color":       color,
		"fields": []map[string]interface{}{
			{"name": "Regle", "value": alert.RuleName, "inline": true},
			{"name": "Severite", "value": alert.Severity, "inline": true},
			{"name": "Date", "value": alert.Timestamp, "inline": false},
			{"name": "Conteneurs", "value": containersStr, "inline": false},
		},
	}

	payload, _ := json.Marshal(map[string]interface{}{"embeds": []interface{}{embed}})
	resp, err := http.Post(webhookURL, "application/json", bytes.NewReader(payload))
	if err != nil {
		return gin.H{"channel": "discord", "success": false, "error": err.Error()}
	}
	defer resp.Body.Close()

	if resp.StatusCode == 200 || resp.StatusCode == 204 {
		return gin.H{"channel": "discord", "success": true, "error": nil}
	}
	return gin.H{"channel": "discord", "success": false, "error": fmt.Sprintf("HTTP %d", resp.StatusCode)}
}

func dispatchEmail(channel AlertChannelConfig, alert AlertData) gin.H {
	cfg := channel.Config
	smtpHost, _ := cfg["smtp_host"].(string)
	smtpPortRaw := cfg["smtp_port"]
	smtpUser, _ := cfg["smtp_user"].(string)
	smtpPass, _ := cfg["smtp_pass"].(string)
	fromAddr, _ := cfg["from_addr"].(string)

	smtpPort := 587
	if p, ok := smtpPortRaw.(float64); ok {
		smtpPort = int(p)
	}

	var toAddrs []string
	if addrs, ok := cfg["to_addrs"].([]interface{}); ok {
		for _, a := range addrs {
			if s, ok := a.(string); ok && s != "" {
				toAddrs = append(toAddrs, s)
			}
		}
	}

	if smtpHost == "" || len(toAddrs) == 0 {
		return gin.H{"channel": "email", "success": false, "error": "Configuration SMTP incomplete"}
	}

	subject := fmt.Sprintf("[AutoDocker] Alerte %s: %s", alert.Severity, alert.RuleName)
	body := fmt.Sprintf("Alerte AutoDocker - %s\n\nRegle: %s\nSeverite: %s\nMessage: %s\nDate: %s\nConteneurs: %s",
		strings.ToUpper(alert.Severity), alert.RuleName, alert.Severity, alert.Message, alert.Timestamp,
		strings.Join(alert.Containers, ", "))

	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s",
		fromAddr, strings.Join(toAddrs, ", "), subject, body)

	addr := fmt.Sprintf("%s:%d", smtpHost, smtpPort)
	var auth smtp.Auth
	if smtpUser != "" && smtpPass != "" {
		auth = smtp.PlainAuth("", smtpUser, smtpPass, smtpHost)
	}

	err := smtp.SendMail(addr, auth, fromAddr, toAddrs, []byte(msg))
	if err != nil {
		return gin.H{"channel": "email", "success": false, "error": err.Error()}
	}
	return gin.H{"channel": "email", "success": true, "error": nil}
}

// DispatchAlert sends an alert to all enabled channels.
func DispatchAlert(alert AlertData) []gin.H {
	if alert.Timestamp == "" {
		alert.Timestamp = time.Now().Format("2006-01-02 15:04:05")
	}

	cfg := loadAlertConfig()
	var results []gin.H

	dispatchers := map[string]func(AlertChannelConfig, AlertData) gin.H{
		"email":   dispatchEmail,
		"discord": dispatchDiscord,
		"webhook": dispatchWebhook,
	}

	for _, ch := range cfg.Channels {
		if !ch.Enabled {
			continue
		}
		dispatcher, ok := dispatchers[ch.Type]
		if !ok {
			results = append(results, gin.H{"channel": ch.Type, "success": false, "error": "Type de canal inconnu"})
			continue
		}
		result := dispatcher(ch, alert)
		results = append(results, result)
	}
	return results
}

// ---------------------------------------------------------------------------
// Endpoints
// ---------------------------------------------------------------------------

func handleGetAlertChannels(c *gin.Context) {
	cfg := loadAlertConfig()
	c.JSON(http.StatusOK, gin.H{"status": "ok", "config": cfg})
}

func handleSaveAlertChannels(c *gin.Context) {
	var body AlertChannelsFile
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "Corps JSON requis"})
		return
	}

	if len(body.Channels) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "Le champ 'channels' doit etre une liste"})
		return
	}

	if err := saveAlertConfig(body); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": err.Error()})
		return
	}

	s := sessions.Default(c)
	username, _ := s.Get("user").(string)
	db.LogActivity(username, "alert_channels_update", "", "Configuration des canaux d'alerte mise a jour")

	c.JSON(http.StatusOK, gin.H{"status": "ok", "message": "Configuration sauvegardee"})
}

func handleTestAlertChannel(c *gin.Context) {
	var body struct {
		Type string `json:"type"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Type == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "Corps JSON requis avec champ 'type'"})
		return
	}

	dispatchers := map[string]func(AlertChannelConfig, AlertData) gin.H{
		"email":   dispatchEmail,
		"discord": dispatchDiscord,
		"webhook": dispatchWebhook,
	}

	dispatcher, ok := dispatchers[body.Type]
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": fmt.Sprintf("Type de canal invalide: %s", body.Type)})
		return
	}

	cfg := loadAlertConfig()
	channel := getChannel(cfg, body.Type)
	if channel == nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "error": fmt.Sprintf("Canal '%s' non configure", body.Type)})
		return
	}

	testAlert := AlertData{
		RuleName:   "Test de connexion",
		Severity:   "info",
		Message:    "Ceci est un message de test envoye depuis AutoDocker pour verifier la configuration du canal d'alerte.",
		Timestamp:  time.Now().Format("2006-01-02 15:04:05"),
		Containers: []string{"test-container"},
	}

	result := dispatcher(*channel, testAlert)

	s := sessions.Default(c)
	username, _ := s.Get("user").(string)
	success, _ := result["success"].(bool)
	db.LogActivity(username, "alert_channel_test", body.Type,
		fmt.Sprintf("Test du canal '%s' : %s", body.Type, map[bool]string{true: "succes", false: "echec"}[success]))

	status := "ok"
	if !success {
		status = "error"
	}
	c.JSON(http.StatusOK, gin.H{"status": status, "result": result})
}
