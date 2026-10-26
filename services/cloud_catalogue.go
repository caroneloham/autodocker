package services

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
)

type cloudSpec struct {
	Name, Label, Prefix, Base, Model, Format, Role string
	Keyless, Vision                                bool
}

// Only documented public API origins. Credentials are looked up at call time;
// none of the registry, operational settings or capabilities contain key values.
var cloudSpecs = []cloudSpec{
	{Name: "gemini", Label: "Gemini", Prefix: "GEMINI", Base: "https://generativelanguage.googleapis.com/v1beta", Model: "gemini-3.8-flash", Format: "gemini", Role: "generation"},
	{Name: "groq", Label: "Groq", Prefix: "GROQ", Base: "https://api.groq.com/openai/v1", Model: "openai/gpt-oss-120b", Format: "groq", Role: "generation"},
	{Name: "nvidia", Label: "NVIDIA", Prefix: "NVIDIA", Base: "https://integrate.api.nvidia.com/v1", Model: "openai/gpt-oss-20b", Format: "nvidia", Role: "generation"},
	{Name: "cloudflare", Label: "Cloudflare", Prefix: "CLOUDFLARE", Model: "@cf/qwen/qwen2.5-coder-32b-instruct", Format: "cloudflare", Role: "generation"},
	{Name: "openrouter", Label: "OpenRouter", Prefix: "OPENROUTER", Base: "https://openrouter.ai/api/v1", Model: "openrouter/free", Format: "openai", Role: "generation"},
	{Name: "kilo", Label: "Kilo", Prefix: "KILO", Base: "https://api.kilo.ai/api/gateway", Model: "kilo-auto/free", Format: "openai", Role: "generation", Keyless: true},
	{Name: "ovh", Label: "OVHcloud", Prefix: "OVH", Base: "https://oai.endpoints.kepler.ai.cloud.ovh.net/v1", Model: "gpt-oss-120b", Format: "ovh", Role: "generation", Keyless: true},
	{Name: "apinex", Label: "APINex", Prefix: "APINEX", Base: "https://api.apinex.bond/v1", Model: "free/gpt-5.6-luna", Format: "openai", Role: "generation"},
	{Name: "cohere", Label: "Cohere", Prefix: "COHERE", Base: "https://api.cohere.ai/compatibility/v1", Model: "command-r7b-12-2024", Format: "cohere", Role: "generation"},
	{Name: "sambanova", Label: "SambaNova", Prefix: "SAMBANOVA", Base: "https://api.sambanova.ai/v1", Model: "Meta-Llama-3.3-70B-Instruct", Format: "openai", Role: "generation"},
	{Name: "onomeo", Label: "Onomeo", Prefix: "ONOMEO", Base: "https://onomeo.com/v1", Model: "auto", Format: "openai", Role: "generation"},
	{Name: "ollama-cloud", Label: "Ollama Cloud", Prefix: "OLLAMA", Base: "https://ollama.com/v1", Model: "gemma4:31b", Format: "openai", Role: "generation", Vision: true},
	{Name: "electronhub", Label: "ElectronHub", Prefix: "ELECTRONHUB", Base: "https://api.electronhub.ai/v1", Model: "gpt-4o-mini", Format: "openai", Role: "generation"},
	{Name: "orcarouter", Label: "OrcaRouter", Prefix: "ORCAROUTER", Base: "https://api.orcarouter.ai/v1", Model: "orcarouter/auto", Format: "openai", Role: "generation"},
	{Name: "llm7", Label: "LLM7", Prefix: "LLM7", Base: "https://api.llm7.io/v1", Model: "default", Format: "openai", Role: "generation"},
	{Name: "aion", Label: "Aion Labs", Prefix: "AION", Base: "https://api.aionlabs.ai/v1", Model: "aion-labs/aion-3.0-mini", Format: "aion", Role: "generation"},
	{Name: "router9", Label: "Router9", Prefix: "ROUTER9", Base: "https://api.router9.com/v1", Model: "deepseek/deepseek-v4-flash", Format: "openai", Role: "generation"},
	{Name: "xkiro", Label: "xKiro", Prefix: "XKIRO", Base: "https://api.xkiro.com/v1", Model: "qwen/qwen3.8-max:free", Format: "openai", Role: "generation"},
	{Name: "zai", Label: "Z.AI", Prefix: "ZAI", Base: "https://api.z.ai/api/paas/v4", Model: "glm-5.3-flash", Format: "zai", Role: "generation"},
	{Name: "tavily", Label: "Tavily", Prefix: "TAVILY", Base: "https://api.tavily.com", Role: "search"},
	{Name: "exa", Label: "Exa", Prefix: "EXA", Base: "https://api.exa.ai", Role: "search"},
	{Name: "firecrawl", Label: "Firecrawl", Prefix: "FIRECRAWL", Base: "https://api.firecrawl.dev/v2", Role: "search"},
	{Name: "parallel", Label: "Parallel", Prefix: "PARALLEL", Base: "https://api.parallel.ai/v1", Role: "search"},
	{Name: "serpsearch", Label: "SERP Search", Prefix: "SERPSEARCH", Base: "https://api.serpsearch.com/api/v1", Role: "search"},
	{Name: "moondream", Label: "Moondream", Prefix: "MOONDREAM", Base: "https://api.moondream.ai/v1", Model: "moondream3.1-9B-A2B", Role: "vision"},
}

type CloudProviderConfig struct {
	Model       string `json:"model,omitempty"`
	BaseURL     string `json:"base_url,omitempty"`
	Enabled     *bool  `json:"enabled,omitempty"`
	ProbeStatus string `json:"probe_status,omitempty"`
}

type CloudConfig struct {
	Providers       map[string]CloudProviderConfig `json:"providers"`
	GenerationOrder []string                       `json:"generation_order"`
	SearchOrder     []string                       `json:"search_order"`
	VisionOrder     []string                       `json:"vision_order"`
}

var cloudModelID = regexp.MustCompile(`^[A-Za-z0-9@][A-Za-z0-9_./:@+\-]{0,199}$`)
var cloudStatusID = regexp.MustCompile(`^[a-z_]{0,50}$`)

func cloudSpecFor(name string) (cloudSpec, bool) {
	for _, spec := range cloudSpecs {
		if spec.Name == name {
			return spec, true
		}
	}
	return cloudSpec{}, false
}

func cloudHasRole(spec cloudSpec, role string) bool {
	return spec.Role == role || role == "vision" && spec.Vision
}

func readCloudConfig() (CloudConfig, error) {
	config := CloudConfig{}
	path := strings.TrimSpace(os.Getenv("AUTODOCKER_CLOUD_CONFIG"))
	if path == "" {
		return config, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return config, fmt.Errorf("configuration des API illisible")
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Size() > 128<<10 {
		return config, fmt.Errorf("configuration des API invalide")
	}
	decoder := json.NewDecoder(io.LimitReader(file, (128<<10)+1))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&config) != nil {
		return CloudConfig{}, fmt.Errorf("configuration des API invalide")
	}
	var extra interface{}
	if decoder.Decode(&extra) != io.EOF {
		return CloudConfig{}, fmt.Errorf("configuration des API invalide")
	}
	for name, options := range config.Providers {
		if _, known := cloudSpecFor(name); !known || options.Model != "" && !cloudModelID.MatchString(options.Model) || !cloudStatusID.MatchString(options.ProbeStatus) {
			return CloudConfig{}, fmt.Errorf("profil API invalide")
		}
		if options.BaseURL != "" && validateCloudBase(options.BaseURL) != nil {
			return CloudConfig{}, fmt.Errorf("adresse API invalide")
		}
	}
	for role, order := range map[string][]string{"generation": config.GenerationOrder, "search": config.SearchOrder, "vision": config.VisionOrder} {
		if len(order) > len(cloudSpecs) {
			return CloudConfig{}, fmt.Errorf("liste des API invalide")
		}
		seen := map[string]bool{}
		for _, name := range order {
			spec, ok := cloudSpecFor(name)
			if !ok || !cloudHasRole(spec, role) || seen[name] {
				return CloudConfig{}, fmt.Errorf("rôle API invalide")
			}
			seen[name] = true
		}
	}
	return config, nil
}

func validateCloudBase(base string) error {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(base, "\r\n") {
		return fmt.Errorf("adresse API HTTPS requise")
	}
	ip := net.ParseIP(u.Hostname())
	if strings.EqualFold(u.Hostname(), "localhost") || ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast()) {
		return fmt.Errorf("le fournisseur cloud ne peut pas charger un modèle local")
	}
	return nil
}

func cloudCredentialNames(spec cloudSpec) []string {
	if spec.Name == "cloudflare" {
		return []string{"CLOUDFLARE_API_TOKENS", "CLOUDFLARE_API_TOKEN"}
	}
	if spec.Name == "gemini" {
		return []string{"GEMINI_API_KEYS", "GEMINI_API_KEY", "GEMINI_API_KEY_PROD"}
	}
	return []string{spec.Prefix + "_API_KEYS", spec.Prefix + "_API_KEY"}
}

// Private-file allowlist shared with main; no arbitrary process settings.
func CloudEnvironmentNames() []string {
	seen := map[string]bool{}
	for _, spec := range cloudSpecs {
		for _, name := range append(cloudCredentialNames(spec), spec.Prefix+"_MODEL", spec.Prefix+"_BASE_URL") {
			seen[name] = true
		}
	}
	seen["CLOUDFLARE_ACCOUNT_ID"], seen["OLLAMA_CLOUD_MODEL"] = true, true
	keys := []string{}
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func cloudSettings(spec cloudSpec, config CloudConfig) (base, model string, err error) {
	base = infrastructureEnvDefault(spec.Prefix+"_BASE_URL", spec.Base)
	model = infrastructureEnvDefault(spec.Prefix+"_MODEL", spec.Model)
	if spec.Name == "ollama-cloud" {
		model = infrastructureEnvDefault("OLLAMA_CLOUD_MODEL", spec.Model)
	}
	if spec.Name == "cloudflare" && base == "" {
		account := strings.TrimSpace(os.Getenv("CLOUDFLARE_ACCOUNT_ID"))
		if !regexp.MustCompile(`^[a-fA-F0-9]{32}$`).MatchString(account) {
			return "", "", fmt.Errorf("compte Cloudflare non configuré")
		}
		base = "https://api.cloudflare.com/client/v4/accounts/" + account + "/ai/v1"
	}
	options := config.Providers[spec.Name]
	if options.BaseURL != "" {
		base = options.BaseURL
	}
	if options.Model != "" {
		model = options.Model
	}
	if err := validateCloudBase(base); err != nil {
		return "", "", err
	}
	if spec.Role != "search" && !cloudModelID.MatchString(model) {
		return "", "", fmt.Errorf("modèle API non configuré")
	}
	if redactCloudSecrets(model) != model {
		return "", "", fmt.Errorf("modèle API invalide")
	}
	return strings.TrimRight(base, "/"), model, nil
}

func redactCloudSecrets(text string) string {
	for _, spec := range cloudSpecs {
		for _, key := range infrastructureCredentials(spec.Name) {
			if key != "" {
				text = strings.ReplaceAll(text, key, "[SECRET MASQUÉ]")
			}
		}
	}
	return text
}

func cloudConfigured(spec cloudSpec, config CloudConfig) bool {
	if !spec.Keyless {
		return len(infrastructureCredentials(spec.Name)) > 0
	}
	options := config.Providers[spec.Name]
	return options.Enabled != nil && *options.Enabled || os.Getenv(spec.Prefix+"_MODEL") != "" || os.Getenv(spec.Prefix+"_BASE_URL") != ""
}

func cloudEnabled(spec cloudSpec, config CloudConfig) bool {
	options := config.Providers[spec.Name]
	return (options.Enabled == nil || *options.Enabled) && cloudConfigured(spec, config)
}

type CloudCapability struct {
	Provider   string   `json:"provider"`
	Label      string   `json:"label"`
	Roles      []string `json:"roles"`
	Model      string   `json:"model,omitempty"`
	Configured bool     `json:"configured"`
	Enabled    bool     `json:"enabled"`
	Probe      string   `json:"probe_status,omitempty"`
}

func CloudCapabilities() ([]CloudCapability, error) {
	config, err := readCloudConfig()
	if err != nil {
		return nil, err
	}
	result := []CloudCapability{}
	for _, spec := range cloudSpecs {
		_, model, _ := cloudSettings(spec, config)
		roles := []string{spec.Role}
		if spec.Vision {
			roles = append(roles, "vision")
		}
		result = append(result, CloudCapability{Provider: spec.Name, Label: spec.Label, Roles: roles, Model: model, Configured: cloudConfigured(spec, config), Enabled: cloudEnabled(spec, config), Probe: config.Providers[spec.Name].ProbeStatus})
	}
	return result, nil
}

func cloudInfrastructureProvider(name string) (infrastructureProvider, bool) {
	spec, known := cloudSpecFor(name)
	if !known || spec.Role != "generation" {
		return infrastructureProvider{}, false
	}
	config, configErr := readCloudConfig()
	base, model, settingsErr := cloudSettings(spec, config)
	p := infrastructureProvider{name: name, model: model}
	p.complete = func(ctx context.Context, system, message string) (string, error) {
		if configErr != nil {
			return "", configErr
		}
		if settingsErr != nil {
			return "", settingsErr
		}
		if !cloudEnabled(spec, config) {
			return "", fmt.Errorf("fournisseur API non activé")
		}
		endpoint := base + "/chat/completions"
		if spec.Format == "gemini" {
			endpoint = base + "/models/" + url.PathEscape(model) + ":generateContent"
		}
		return requestInfrastructureModel(ctx, endpoint, model, nextInfrastructureCredential(name), spec.Format, system, message)
	}
	return p, true
}
