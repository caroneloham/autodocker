package services

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCloudReasoningSettingsAndActualModelStaySafe(t *testing.T) {
	for _, format := range []string{"cohere", "zai", "aion", "groq", "ovh"} {
		t.Run(format, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]interface{}
				json.NewDecoder(r.Body).Decode(&body)
				wanted := "none"
				if format == "zai" || format == "groq" || format == "ovh" {
					wanted = "low"
				}
				if body["reasoning_effort"] != wanted || body["thinking"] != nil {
					t.Error("unsupported thinking setting would reject the request")
				}
				json.NewEncoder(w).Encode(map[string]interface{}{"model": "actual-model", "choices": []interface{}{map[string]interface{}{"message": map[string]string{"content": "synthetic-answer"}}}})
			}))
			defer server.Close()
			actual := ""
			ctx := context.WithValue(context.Background(), infrastructureModelObserverKey{}, func(model string) { actual = model })
			text, err := requestInfrastructureModel(ctx, server.URL, "openai/gpt-oss-20b", "synthetic-key", format, "system", "message")
			if err != nil || text != "synthetic-answer" || actual != "actual-model" {
				t.Fatalf("response/model lost: %v", err)
			}
		})
	}
}
