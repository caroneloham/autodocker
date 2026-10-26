package services

import (
	"context"
	"strings"
	"testing"
)

func TestDocumentSearchIntentDoesNotTurnEveryPDFServerIntoRAG(t *testing.T) {
	for _, prompt := range []string{"Je veux poser des questions sur mes documents PDF avec des sources", "Une IA RAG privée", "recherche sémantique dans les fichiers"} {
		if !infrastructureDocumentSearchRequested(prompt) {
			t.Errorf("lost search intent: %s", prompt)
		}
	}
	for _, prompt := range []string{"Nextcloud pour partager mes PDF", "Ollama sans RAG", "Un site pour publier des documents", "Open WebUI sans recherche documentaire"} {
		if infrastructureDocumentSearchRequested(prompt) {
			t.Errorf("invented RAG intent: %s", prompt)
		}
	}
}

func TestRAGRequirementAddsTrustedConnectionsButPreservesExplicitExclusions(t *testing.T) {
	p := infrastructureProvider{name: "any-model", complete: func(context.Context, string, string) (string, error) {
		return infrastructureAnswer("open-webui", "ollama"), nil
	}}
	result, err := generateInfrastructureWithProvider(context.Background(), "Open WebUI avec Ollama, pour poser des questions sur mes documents PDF", p)
	if err != nil {
		t.Fatal(err)
	}
	keys := infrastructureKeys(result)
	if !keys["tei"] || !keys["qdrant"] {
		t.Fatal("document search is not connected")
	}
	if len(result.DesignChecks) == 0 || len(result.ManualSteps) == 0 {
		t.Fatal("design validation and remaining work were omitted")
	}
	_, err = generateInfrastructureWithProvider(context.Background(), "Open WebUI pour interroger mes documents avec Ollama, sans Qdrant", p)
	if err == nil || !strings.Contains(err.Error(), "exclu") {
		t.Fatalf("explicit exclusion was overruled: %v", err)
	}
}

func TestQuestionWithPartialArchitectureAlwaysRequiresClarification(t *testing.T) {
	p := infrastructureProvider{name: "test", complete: func(context.Context, string, string) (string, error) {
		return `{"services":[{"key":"nginx"}],"questions":["Quelle application voulez-vous déployer ?"]}`, nil
	}}
	r, err := generateInfrastructureWithProvider(context.Background(), "Une application web partagée", p)
	if err != nil || r.Payload != nil || r.Confidence != "needs_clarification" {
		t.Fatal("an unresolved question became an accepted architecture")
	}
}

func TestRecoveredCatalogueOmissionsRetainRequestedVersions(t *testing.T) {
	p := infrastructureProvider{name: "test", complete: func(context.Context, string, string) (string, error) { return infrastructureAnswer("gitea"), nil }}
	r, err := generateInfrastructureWithProvider(context.Background(), "Gitea avec PostgreSQL version 16 et Redis version 7", p)
	if err != nil {
		t.Fatal(err)
	}
	versions := map[string]string{}
	for _, s := range normalizeServiceList(r.Payload["services"]) {
		key, _ := s["key"].(string)
		versions[key], _ = s["image"].(string)
	}
	if !strings.HasPrefix(versions["postgres"], "postgres:16") || !strings.HasPrefix(versions["redis"], "redis:7") {
		t.Fatalf("lost requested versions: %v", versions)
	}
}
