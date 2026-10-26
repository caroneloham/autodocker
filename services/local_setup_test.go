package services

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLocalSetupSecretNeverLeavesServer(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AUTODOCKER_ROOT", root)
	secret := "synthetic-test-key-never-publish"
	setup, err := SaveLocalSetup(LocalSetupInput{LocalSetup: LocalSetup{Provider: "api", APIProvider: "gemini", Model: "gemini-test-model", DockerHost: "auto"}, APIKey: secret})
	if err != nil {
		t.Fatal(err)
	}
	if !setup.HasAPIKey {
		t.Fatal("key flag missing")
	}
	public, err := ReadLocalSetup()
	if err != nil {
		t.Fatal(err)
	}
	response, _ := json.Marshal(public)
	file, err := os.ReadFile(filepath.Join(root, "secrets", "local_setup.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(response), secret) || strings.Contains(string(file), secret) {
		t.Fatal("API key leaked in response or file")
	}
	if runtime.GOOS != "windows" {
		for _, path := range []string{"local_setup.json", ".local_setup.key"} {
			info, err := os.Stat(filepath.Join(root, "secrets", path))
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("%s must be private", path)
			}
		}
	}
	provider, configured, err := localSetupInfrastructureProvider()
	if err != nil || !configured || provider.name != "gemini" {
		t.Fatal("configured provider unavailable", err)
	}
	// Saving unrelated Docker settings must preserve the encrypted key.
	public.DockerHost = "auto"
	updated, err := SaveLocalSetup(LocalSetupInput{LocalSetup: public})
	if err != nil || !updated.HasAPIKey {
		t.Fatal("key lost on update", err)
	}
	changed := LocalSetupInput{LocalSetup: LocalSetup{Provider: "api", APIProvider: "openrouter", Model: "test-model", DockerHost: "auto"}}
	if ok, _ := ProbeLocalAIInput(changed); ok {
		t.Fatal("old vendor key accepted by probe")
	}
	if _, err := SaveLocalSetup(changed); err == nil {
		t.Fatal("old vendor key reused")
	}
}

func TestLocalAIProbeChecksSelectedModel(t *testing.T) {
	t.Setenv("AUTODOCKER_ROOT", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"name":"local-test:latest"}]}`))
	}))
	defer server.Close()
	input := LocalSetupInput{LocalSetup: LocalSetup{Provider: "ollama", BaseURL: server.URL, Model: "local-test", DockerHost: "auto"}}
	if ok, message := ProbeLocalAIInput(input); !ok {
		t.Fatalf("loaded model was not detected: %s", message)
	}
	input.Model = "missing-model"
	if ok, _ := ProbeLocalAIInput(input); ok {
		t.Fatal("missing model reported as ready")
	}
}

func TestLocalSetupRejectsRemoteEndpointsAndDockerTCP(t *testing.T) {
	for _, endpoint := range []string{"http://example.com:11434", "https://127.0.0.1:11434", "http://127.0.0.1:11434/private", "http://user:pass@127.0.0.1:11434", "http://127.0.0.1:11434/?token=x"} {
		if _, err := ValidateLocalAIBase(endpoint, "11434"); err == nil {
			t.Errorf("accepted unsafe endpoint %q", endpoint)
		}
	}
	if _, err := ValidateLocalDockerHost("tcp://127.0.0.1:2375"); err == nil {
		t.Fatal("Docker TCP accepted")
	}
}
