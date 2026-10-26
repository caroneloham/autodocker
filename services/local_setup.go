package services

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LocalSetup is configuration for one locally installed AutoDocker instance.
// Only the authenticated owner may change it; API keys never leave this package.
type LocalSetup struct {
	Provider    string `json:"provider"`
	BaseURL     string `json:"base_url"`
	Model       string `json:"model"`
	APIProvider string `json:"api_provider"`
	DockerHost  string `json:"docker_host"`
	HasAPIKey   bool   `json:"has_api_key"`
}

type LocalSetupInput struct {
	LocalSetup
	APIKey       string `json:"api_key"`
	RemoveAPIKey bool   `json:"remove_api_key"`
}

type storedLocalSetup struct {
	LocalSetup
	EncryptedKey string `json:"encrypted_api_key,omitempty"`
	KeyProvider  string `json:"key_provider,omitempty"`
}

var localSetupLock sync.Mutex

type localSetupProviderContextKey struct{}

// CaptureLocalSetupProvider freezes the installation's selected provider for
// an asynchronous job. The returned context is independent of the HTTP request.
func CaptureLocalSetupProvider() (context.Context, error) {
	provider, configured, err := localSetupInfrastructureProvider()
	if err != nil {
		return nil, err
	}
	if !configured {
		return context.Background(), nil
	}
	return context.WithValue(context.Background(), localSetupProviderContextKey{}, provider), nil
}

func localSetupPath() string    { return filepath.Join(secretsDir(), "local_setup.json") }
func localSetupKeyPath() string { return filepath.Join(secretsDir(), ".local_setup.key") }

func readStoredLocalSetup() (storedLocalSetup, error) {
	var setup storedLocalSetup
	data, err := os.ReadFile(localSetupPath())
	if errors.Is(err, os.ErrNotExist) {
		return setup, nil
	}
	if err != nil {
		return setup, err
	}
	if len(data) > 8192 || json.Unmarshal(data, &setup) != nil {
		return storedLocalSetup{}, errors.New("configuration locale invalide")
	}
	setup.HasAPIKey = setup.EncryptedKey != "" && (setup.Provider != "api" || setup.APIProvider == setup.KeyProvider)
	return setup, nil
}

func ReadLocalSetup() (LocalSetup, error) {
	localSetupLock.Lock()
	defer localSetupLock.Unlock()
	setup, err := readStoredLocalSetup()
	return setup.LocalSetup, err
}

func ValidateLocalAIBase(raw string, defaultPort string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		raw = "http://127.0.0.1:" + defaultPort
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return "", errors.New("adresse IA locale invalide")
	}
	name := u.Hostname()
	if name == "" || strings.EqualFold(name, "localhost") == false && (net.ParseIP(name) == nil || !net.ParseIP(name).IsLoopback()) {
		return "", errors.New("l’IA doit utiliser localhost ou une adresse de boucle locale")
	}
	port := u.Port()
	if port == "" {
		port = defaultPort
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return "", errors.New("port IA local invalide")
	}
	if strings.EqualFold(name, "localhost") {
		name = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(name, port), nil
}

func ValidateLocalDockerHost(host string) (string, error) {
	host = strings.TrimSpace(host)
	if host == "" || host == "auto" {
		return "auto", nil
	}
	if runtime.GOOS == "windows" {
		if host == "npipe:////./pipe/docker_engine" || host == "npipe:////./pipe/dockerDesktopLinuxEngine" {
			return host, nil
		}
		return "", errors.New("sélectionnez une connexion Docker Desktop locale")
	}
	if !strings.HasPrefix(host, "unix:///") {
		return "", errors.New("sélectionnez un socket Docker Unix local")
	}
	path := strings.TrimPrefix(host, "unix://")
	if filepath.Clean(path) != path || !filepath.IsAbs(path) || len(path) > 240 || strings.ContainsAny(path, "\x00\r\n") {
		return "", errors.New("socket Docker local invalide")
	}
	return host, nil
}

func validateLocalSetup(input LocalSetupInput) (storedLocalSetup, error) {
	setup := storedLocalSetup{LocalSetup: input.LocalSetup}
	switch setup.Provider {
	case "", "ollama", "vllm", "lmstudio", "api":
	default:
		return setup, errors.New("moteur IA non pris en charge")
	}
	if len(setup.Model) > 180 || strings.ContainsAny(setup.Model, "\r\n\x00") {
		return setup, errors.New("modèle IA invalide")
	}
	setup.Model = strings.TrimSpace(setup.Model)
	switch setup.Provider {
	case "ollama":
		var err error
		setup.BaseURL, err = ValidateLocalAIBase(setup.BaseURL, "11434")
		if err != nil {
			return setup, err
		}
		if setup.Model == "" {
			setup.Model = DefaultLocalAIModel
		}
		setup.APIProvider = ""
	case "vllm":
		var err error
		setup.BaseURL, err = ValidateLocalAIBase(setup.BaseURL, "8000")
		if err != nil {
			return setup, err
		}
		if setup.Model == "" {
			return setup, errors.New("indiquez le modèle chargé dans vLLM")
		}
		setup.APIProvider = ""
	case "lmstudio":
		var err error
		setup.BaseURL, err = ValidateLocalAIBase(setup.BaseURL, "1234")
		if err != nil {
			return setup, err
		}
		if setup.Model == "" {
			return setup, errors.New("indiquez le modèle chargé dans LM Studio")
		}
		setup.APIProvider = ""
	case "api":
		setup.BaseURL = ""
		if setup.APIProvider != "gemini" && setup.APIProvider != "openrouter" {
			return setup, errors.New("choisissez Gemini ou OpenRouter")
		}
		if setup.Model == "" {
			return setup, errors.New("indiquez un modèle pour cette API")
		}
	default:
		setup.BaseURL, setup.Model, setup.APIProvider = "", "", ""
	}
	var err error
	setup.DockerHost, err = ValidateLocalDockerHost(setup.DockerHost)
	if err != nil {
		return setup, err
	}
	if len(input.APIKey) > 4096 || strings.ContainsAny(input.APIKey, "\r\n\x00") {
		return setup, errors.New("clé API invalide")
	}
	return setup, nil
}

func SaveLocalSetup(input LocalSetupInput) (LocalSetup, error) {
	localSetupLock.Lock()
	defer localSetupLock.Unlock()
	setup, err := validateLocalSetup(input)
	if err != nil {
		return LocalSetup{}, err
	}
	old, err := readStoredLocalSetup()
	if err != nil {
		return LocalSetup{}, err
	}
	if input.RemoveAPIKey {
		setup.EncryptedKey = ""
		setup.KeyProvider = ""
	} else if input.APIKey != "" {
		setup.EncryptedKey, err = encryptLocalSetupKey(input.APIKey)
		if err != nil {
			return LocalSetup{}, err
		}
		setup.KeyProvider = setup.APIProvider
	} else {
		if setup.Provider != "api" || old.KeyProvider == setup.APIProvider {
			setup.EncryptedKey = old.EncryptedKey
			setup.KeyProvider = old.KeyProvider
		}
	}
	if setup.Provider == "api" && setup.EncryptedKey == "" {
		return LocalSetup{}, errors.New("saisissez une clé API")
	}
	setup.HasAPIKey = setup.EncryptedKey != ""
	data, err := json.Marshal(setup)
	if err != nil {
		return LocalSetup{}, err
	}
	if err := os.MkdirAll(secretsDir(), 0700); err != nil {
		return LocalSetup{}, err
	}
	file, err := os.CreateTemp(secretsDir(), ".local-setup-*.tmp")
	if err != nil {
		return LocalSetup{}, err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return LocalSetup{}, err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return LocalSetup{}, err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return LocalSetup{}, err
	}
	if err := file.Close(); err != nil {
		return LocalSetup{}, err
	}
	if err := os.Rename(file.Name(), localSetupPath()); err != nil {
		return LocalSetup{}, err
	}
	return setup.LocalSetup, nil
}

func localEncryptionKey(create bool) ([]byte, error) {
	data, err := os.ReadFile(localSetupKeyPath())
	if err == nil {
		if len(data) != 32 {
			return nil, errors.New("clé locale invalide")
		}
		return data, nil
	}
	if !create || !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	data = make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(secretsDir(), 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(localSetupKeyPath(), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return nil, err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	return data, nil
}

func encryptLocalSetupKey(plaintext string) (string, error) {
	key, err := localEncryptionKey(true)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(nonce, nonce, []byte(plaintext), []byte("autodocker-local-ai-key-v1"))
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func decryptLocalSetupKey(ciphertext string) (string, error) {
	key, err := localEncryptionKey(false)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	data, err := base64.RawURLEncoding.DecodeString(ciphertext)
	if err != nil || len(data) < aead.NonceSize() {
		return "", errors.New("clé API locale invalide")
	}
	plain, err := aead.Open(nil, data[:aead.NonceSize()], data[aead.NonceSize():], []byte("autodocker-local-ai-key-v1"))
	if err != nil {
		return "", errors.New("clé API locale indisponible")
	}
	return string(plain), nil
}

func localSetupInfrastructureProvider() (infrastructureProvider, bool, error) {
	localSetupLock.Lock()
	setup, err := readStoredLocalSetup()
	localSetupLock.Unlock()
	if err != nil || setup.Provider == "" {
		return infrastructureProvider{}, false, err
	}
	key := ""
	if setup.Provider == "api" {
		key, err = decryptLocalSetupKey(setup.EncryptedKey)
		if err != nil {
			return infrastructureProvider{}, false, errors.New("clé API locale indisponible")
		}
	}
	name := setup.Provider
	if name == "api" {
		name = setup.APIProvider
	}
	provider := infrastructureProvider{name: name, model: setup.Model}
	provider.complete = func(ctx context.Context, system, prompt string) (string, error) {
		endpoint, format := "", "openai"
		switch setup.Provider {
		case "ollama":
			endpoint, format = setup.BaseURL+"/api/chat", "ollama"
		case "vllm":
			endpoint = setup.BaseURL + "/v1/chat/completions"
		case "lmstudio":
			endpoint, format = setup.BaseURL+"/v1/chat/completions", "lmstudio"
		case "api":
			if setup.APIProvider == "gemini" {
				endpoint, format = "https://generativelanguage.googleapis.com/v1beta/models/"+url.PathEscape(setup.Model)+":generateContent", "gemini"
			} else {
				endpoint = "https://openrouter.ai/api/v1/chat/completions"
			}
		}
		return requestInfrastructureModel(ctx, endpoint, setup.Model, key, format, system, prompt)
	}
	return provider, true, nil
}

// ProbeLocalAI checks only a fixed discovery endpoint and never sends an API key.
func ProbeLocalAI() (bool, string) {
	setup, err := ReadLocalSetup()
	if err != nil {
		return false, "Configuration locale indisponible."
	}
	return probeLocalAI(setup)
}

func ProbeLocalAIInput(input LocalSetupInput) (bool, string) {
	validated, err := validateLocalSetup(input)
	if err != nil {
		return false, err.Error()
	}
	if validated.Provider == "api" {
		localSetupLock.Lock()
		current, err := readStoredLocalSetup()
		localSetupLock.Unlock()
		if err != nil {
			return false, "Configuration locale indisponible."
		}
		validated.HasAPIKey = !input.RemoveAPIKey && (input.APIKey != "" || current.EncryptedKey != "" && current.KeyProvider == validated.APIProvider)
	}
	return probeLocalAI(validated.LocalSetup)
}

func probeLocalAI(setup LocalSetup) (bool, string) {
	if setup.Provider == "" {
		return false, "Choisissez un moteur IA."
	}
	if setup.Provider == "api" {
		if !setup.HasAPIKey {
			return false, "Saisissez une clé API."
		}
		return true, "Configuration valide. La première demande IA vérifiera la clé auprès du fournisseur."
	}
	path := "/v1/models"
	if setup.Provider == "ollama" {
		path = "/api/tags"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, setup.BaseURL+path, nil)
	client := &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return false, "Moteur local injoignable. Vérifiez qu’il est démarré."
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return false, fmt.Sprintf("Moteur local : réponse HTTP %d.", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return false, "La liste des modèles est indisponible."
	}
	var catalogue struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &catalogue) != nil {
		return false, "La liste des modèles est invalide."
	}
	available := false
	for _, item := range catalogue.Models {
		if item.Name == setup.Model || strings.TrimSuffix(item.Name, ":latest") == setup.Model {
			available = true
		}
	}
	for _, item := range catalogue.Data {
		if item.ID == setup.Model {
			available = true
		}
	}
	if !available {
		return false, "Moteur détecté, mais ce modèle n’est pas chargé."
	}
	return true, "Moteur local détecté."
}
