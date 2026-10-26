package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeSyntheticEnv(t *testing.T, directory, name, content string) string {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func unsetSyntheticEnv(t *testing.T, key string) {
	t.Helper()
	// Setenv registers cleanup of the original process value before removing it.
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}

func TestLoadEnvFilePreservesExplicitRuntimeValues(t *testing.T) {
	t.Setenv("PORT", "18080")
	t.Setenv("BIND_HOST", "127.0.0.1")
	t.Setenv("AUTODOCKER_ROOT", "runtime-root")
	t.Setenv("AUTODOCKER_ENV_TEST_VALUE", "process-value")
	path := writeSyntheticEnv(t, t.TempDir(), ".env", "PORT=8000\nBIND_HOST=0.0.0.0\nAUTODOCKER_ROOT=file-root\nAUTODOCKER_ENV_TEST_VALUE=file-value\n")
	loaded, ok := loadEnvFile(path)
	if !ok || loaded != 1 {
		t.Fatalf("load result = %d, %v; want one non-runtime value", loaded, ok)
	}
	for key, want := range map[string]string{
		"PORT": "18080", "BIND_HOST": "127.0.0.1", "AUTODOCKER_ROOT": "runtime-root",
		"AUTODOCKER_ENV_TEST_VALUE": "file-value",
	} {
		if got := os.Getenv(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestLauncherSecurityFlagsCannotBeOverriddenByEnvFile(t *testing.T) {
	values := map[string]string{"AUTH_BYPASS": "0", "DEMO": "0", "AUTODOCKER_GENERATOR_ONLY": "0", "AUTODOCKER_LOCAL_BOOTSTRAP": "1", "AUTODOCKER_LOCAL_HTTP": "1"}
	for key, value := range values {
		t.Setenv(key, value)
	}
	path := writeSyntheticEnv(t, t.TempDir(), "autodocker.env", "AUTH_BYPASS=1\nDEMO=1\nAUTODOCKER_GENERATOR_ONLY=1\nAUTODOCKER_LOCAL_BOOTSTRAP=0\nAUTODOCKER_LOCAL_HTTP=0\n")
	if loaded, ok := loadEnvFile(path); !ok || loaded != 0 {
		t.Fatalf("security overrides loaded: %d %v", loaded, ok)
	}
	for key, want := range values {
		if got := os.Getenv(key); got != want {
			t.Errorf("%s=%q, want %q", key, got, want)
		}
	}
}

func TestLoadEnvFilesKeepFilePrecedenceWithoutRuntimeOverrides(t *testing.T) {
	for _, key := range []string{"PORT", "BIND_HOST", "AUTODOCKER_ROOT"} {
		unsetSyntheticEnv(t, key)
	}
	runtimeOverrides := explicitRuntimeEnvKeys()
	directory := t.TempDir()
	first := writeSyntheticEnv(t, directory, ".env", "PORT=8000\nBIND_HOST=0.0.0.0\nAUTODOCKER_ROOT=first-root\n")
	second := writeSyntheticEnv(t, directory, "autodocker.env", "PORT=18080\nBIND_HOST=127.0.0.1\nAUTODOCKER_ROOT=second-root\n")
	for _, path := range []string{first, second} {
		if loaded, ok := loadEnvFileWithRuntimeKeys(path, runtimeOverrides); !ok || loaded != 3 {
			t.Fatalf("load result = %d, %v; want all three file defaults", loaded, ok)
		}
	}
	for key, want := range map[string]string{"PORT": "18080", "BIND_HOST": "127.0.0.1", "AUTODOCKER_ROOT": "second-root"} {
		if got := os.Getenv(key); got != want {
			t.Errorf("%s = %q, want later file value %q", key, got, want)
		}
	}
}

func TestLoadEnvFileKeepsAIOverrideAndAliasBehavior(t *testing.T) {
	for _, key := range []string{"GEMINI_API_KEYS", "OPENROUTER_API_KEYS", "OPENROUTER_API_KEY", "OPEN_ROUTER_API_KEYS", "OPEN_ROUTER_API_KEY"} {
		t.Setenv(key, "")
	}
	t.Setenv("GEMINI_API_KEY", "synthetic-process-value")
	path := writeSyntheticEnv(t, t.TempDir(), ".env", "GEMINI_API_KEY=synthetic-file-value\nOPEN_ROUTER_API_KEY=synthetic-router-value\n")
	if loaded, ok := loadEnvFile(path); !ok || loaded != 2 {
		t.Fatalf("load result = %d, %v; want both AI values loaded", loaded, ok)
	}
	normalizeAIEnvAliases()
	if os.Getenv("GEMINI_API_KEY") != "synthetic-file-value" || os.Getenv("GEMINI_API_KEYS") != "synthetic-file-value" {
		t.Error("the file must still replace AI values and populate the plural alias")
	}
	if os.Getenv("OPENROUTER_API_KEYS") != "synthetic-router-value" {
		t.Error("the OpenRouter alias must remain supported")
	}
}
