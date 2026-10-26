package services

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func cloudTestConfig(t *testing.T, config CloudConfig) {
	t.Helper()
	data, _ := json.Marshal(config)
	path := filepath.Join(t.TempDir(), "cloud.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTODOCKER_CLOUD_CONFIG", path)
}

func TestCloudRegistrySeparatesRolesAndDoesNotExposeSecrets(t *testing.T) {
	yes := true
	t.Setenv("OPENROUTER_API_KEYS", "")
	t.Setenv("OPENROUTER_API_KEY", "synthetic-cloud-key")
	t.Setenv("TAVILY_API_KEYS", "")
	t.Setenv("TAVILY_API_KEY", "synthetic-search-key")
	cloudTestConfig(t, CloudConfig{Providers: map[string]CloudProviderConfig{"openrouter": {Model: "cohere/north-mini-code:free"}, "kilo": {Enabled: &yes}}, GenerationOrder: []string{"openrouter", "kilo"}, SearchOrder: []string{"tavily"}})
	providers, err := infrastructureAutoProviders()
	if err != nil || len(providers) != 2 || providers[0].name != "openrouter" || providers[0].model != "cohere/north-mini-code:free" || providers[1].name != "kilo" {
		t.Fatalf("new generation routes missing: %v", err)
	}
	capabilities, err := CloudCapabilities()
	if err != nil || len(capabilities) != 25 {
		t.Fatal("capability inventory incomplete")
	}
	raw, _ := json.Marshal(capabilities)
	if strings.Contains(string(raw), "synthetic-") || strings.Contains(string(raw), "api_key") || strings.Contains(string(raw), "base_url") {
		t.Fatal("private configuration leaked through capabilities")
	}
	if _, _, err := cloudToolProviders("search", "openrouter"); err == nil {
		t.Fatal("generator used as a search provider")
	}
	if _, _, err := cloudToolProviders("vision", "ollama-cloud"); err == nil {
		t.Fatal("unconfigured cloud vision accepted")
	}
}

func TestCloudConfigRejectsLocalModelsUnknownFieldsAndRoles(t *testing.T) {
	for _, base := range []string{"http://127.0.0.1:1234", "https://127.0.0.2", "https://10.0.0.2", "https://localhost", "https://user:secret@example.org", "https://example.org?key=secret"} {
		if validateCloudBase(base) == nil {
			t.Fatal("unsafe cloud endpoint accepted")
		}
	}
	for _, data := range []string{`{"providers":{},"AUTH_BYPASS":1}`, `{"generation_order":["tavily"]}`, `{"search_order":["local"]}`, `{"providers":{"local":{"enabled":true}}}`} {
		path := filepath.Join(t.TempDir(), "bad.json")
		os.WriteFile(path, []byte(data), 0600)
		t.Setenv("AUTODOCKER_CLOUD_CONFIG", path)
		if _, err := readCloudConfig(); err == nil {
			t.Fatal("invalid operational configuration accepted")
		}
	}
}

func TestCloudResponseModelMetadataIsRequestScoped(t *testing.T) {
	provider := infrastructureProvider{name: "openrouter", model: "configured-model"}
	provider.complete = func(ctx context.Context, _, _ string) (string, error) {
		ctx.Value(infrastructureModelObserverKey{}).(func(string))("actual-model")
		return infrastructureAnswer("nginx"), nil
	}
	result, err := generateInfrastructureWithProvider(context.Background(), "Nginx seul", provider)
	if err != nil || result.Model != "actual-model" || provider.model != "configured-model" {
		t.Fatal("actual model metadata was lost or mutated shared provider state")
	}
}
