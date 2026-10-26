package services

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Read the model's advertised capabilities rather than guessing from its name.
// Some thinking models ignore generic chat-template kwargs and consume the
// complete output budget without producing an answer. LM Studio's native API
// exposes an explicit per-request control, which does not alter user settings.
func lmStudioInfrastructureReasoning(ctx context.Context, base, model string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/models", nil)
	if err != nil {
		return ""
	}
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ""
	}
	var result struct {
		Models []struct {
			Key    string `json:"key"`
			Loaded []struct {
				ID string `json:"id"`
			} `json:"loaded_instances"`
			Capabilities struct {
				Reasoning struct {
					Allowed []string `json:"allowed_options"`
				} `json:"reasoning"`
			} `json:"capabilities"`
		} `json:"models"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&result) != nil {
		return ""
	}
	requested := strings.TrimSpace(os.Getenv("AUTODOCKER_AI_REASONING"))
	if requested == "auto" {
		return ""
	}
	for _, entry := range result.Models {
		matches := entry.Key == model
		for _, loaded := range entry.Loaded {
			if loaded.ID == model {
				matches = true
			}
		}
		if !matches {
			continue
		}
		if requested != "" && containsInfrastructureString(entry.Capabilities.Reasoning.Allowed, requested) {
			return requested
		}
		for _, option := range []string{"off", "low"} {
			if containsInfrastructureString(entry.Capabilities.Reasoning.Allowed, option) {
				return option
			}
		}
	}
	return ""
}
