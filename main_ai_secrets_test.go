package main

import (
	"os"
	"testing"
)

func TestPrivateAISecretsCannotDisableAuthOrChangeRoot(t *testing.T) {
	for _, key := range []string{"GROQ_API_KEY", "GROQ_MODEL", "AUTH_BYPASS", "BIND_HOST", "AUTODOCKER_ROOT"} {
		t.Setenv(key, "original")
	}
	path := writeSyntheticEnv(t, t.TempDir(), "keys.env", "GROQ_API_KEY=\"synthetic-secret\"\nGROQ_MODEL=synthetic-model\nAUTH_BYPASS=1\nBIND_HOST=0.0.0.0\nAUTODOCKER_ROOT=elsewhere\n")
	if err := loadAISecretsFile(path); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("GROQ_API_KEY") != "synthetic-secret" || os.Getenv("GROQ_MODEL") != "synthetic-model" {
		t.Fatal("AI configuration was not loaded")
	}
	for _, key := range []string{"AUTH_BYPASS", "BIND_HOST", "AUTODOCKER_ROOT"} {
		if os.Getenv(key) != "original" {
			t.Fatalf("private key file changed %s", key)
		}
	}
}

func TestPrivateAISecretsRejectsMissingOrUnrecognizedFiles(t *testing.T) {
	if err := loadAISecretsFile(""); err != nil {
		t.Fatal(err)
	}
	if err := loadAISecretsFile(t.TempDir() + "/missing.env"); err == nil {
		t.Fatal("missing requested file accepted")
	}
	path := writeSyntheticEnv(t, t.TempDir(), "keys.env", "AUTH_BYPASS=1\n")
	if err := loadAISecretsFile(path); err == nil {
		t.Fatal("unrecognized file accepted")
	}
}

func TestPrivateAIKeysReplaceStalePluralAlias(t *testing.T) {
	t.Setenv("GEMINI_API_KEYS", "synthetic-old")
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GEMINI_API_KEY_PROD", "synthetic-old-prod")
	path := writeSyntheticEnv(t, t.TempDir(), "keys.env", "GEMINI_API_KEY=synthetic-new\n")
	if err := loadAISecretsFile(path); err != nil {
		t.Fatal(err)
	}
	normalizeAIEnvAliases()
	if os.Getenv("GEMINI_API_KEYS") != "synthetic-new" || os.Getenv("GEMINI_API_KEY_PROD") != "" {
		t.Fatal("stale credential was retained")
	}
}

func TestExpandedCloudKeysRemainServerSideAndReplaceOldAliases(t *testing.T) {
	for _, key := range []string{"OPENROUTER_API_KEYS", "OPENROUTER_API_KEY", "COHERE_API_KEY", "TAVILY_API_KEY", "MOONDREAM_API_KEY", "AUTH_BYPASS"} {
		t.Setenv(key, "old-value")
	}
	path := writeSyntheticEnv(t, t.TempDir(), "expanded.env", "OPENROUTER_API_KEY=synthetic-new-router\nCOHERE_API_KEY=synthetic-cohere\nTAVILY_API_KEY=synthetic-search\nMOONDREAM_API_KEY=synthetic-vision\nAUTH_BYPASS=1\n")
	if err := loadAISecretsFile(path); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("OPENROUTER_API_KEYS") != "" || os.Getenv("OPENROUTER_API_KEY") != "synthetic-new-router" || os.Getenv("COHERE_API_KEY") != "synthetic-cohere" || os.Getenv("TAVILY_API_KEY") != "synthetic-search" || os.Getenv("MOONDREAM_API_KEY") != "synthetic-vision" || os.Getenv("AUTH_BYPASS") != "old-value" {
		t.Fatal("expanded private loader retained stale aliases or changed security settings")
	}
}
