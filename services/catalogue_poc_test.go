package services

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/goccy/go-yaml"
)

func TestPOCAllCatalogueComposeFiles(t *testing.T) {
	input := os.Getenv("AUTODOCKER_CATALOGUE_POC_INPUT")
	if input == "" {
		t.Skip("catalogue integration evaluation is opt-in")
	}
	data, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Key     string              `json:"key"`
		Payload BuildComposePayload `json:"payload"`
	}
	if err = json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	results := []M{}
	images := map[string]string{}
	for _, tc := range cases {
		t.Run(tc.Key, func(t *testing.T) {
			if tc.Key == "moodle" {
				results = append(results, M{"key": tc.Key, "disabled": true, "reason": "reference image removed upstream"})
				t.Skip("Unavailable profile is disabled before generation")
			}
			if err := ValidateInfrastructureOptions(tc.Payload); err != nil {
				t.Fatal(err)
			}
			c := BuildCompose(tc.Payload, "catalogue-"+tc.Key)
			services := c["services"].(M)
			if len(services) == 0 {
				t.Fatal("empty catalogue service")
			}
			for key, raw := range services {
				svc := raw.(M)
				if image, ok := svc["image"].(string); ok {
					images[key] = image
				}
			}
			encoded, err := yaml.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "compose.yaml")
			if err = os.WriteFile(path, encoded, 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("docker", "compose", "-f", path, "config", "--quiet")
			output, err := cmd.CombinedOutput()
			results = append(results, M{"key": tc.Key, "passed": err == nil})
			if err != nil {
				t.Fatalf("compose rejected: %s", output)
			}
		})
	}
	if path := os.Getenv("AUTODOCKER_CATALOGUE_POC_REPORT"); path != "" {
		encoded, _ := json.MarshalIndent(M{"cases": results, "images": images}, "", "  ")
		if err := os.WriteFile(path, encoded, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
