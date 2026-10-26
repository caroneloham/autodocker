package services

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/png"
	"strings"
	"testing"
)

func isolateCloudToolTest(t *testing.T) {
	t.Helper()
	oldRequest, oldRouter := cloudToolRequest, infrastructureAutoRouter
	infrastructureAutoRouter = newInfrastructureRouter()
	t.Cleanup(func() { cloudToolRequest, infrastructureAutoRouter = oldRequest, oldRouter })
}

func TestCloudSearchUsesCorrectProtocolAndFiltersUntrustedResults(t *testing.T) {
	isolateCloudToolTest(t)
	t.Setenv("PARALLEL_API_KEY", "synthetic-tool-key")
	t.Setenv("PARALLEL_API_KEYS", "")
	cloudTestConfig(t, CloudConfig{SearchOrder: []string{"parallel"}})
	cloudToolRequest = func(_ context.Context, method, endpoint, key, header string, body interface{}) (map[string]interface{}, error) {
		params := body.(map[string]interface{})
		if method != "POST" || endpoint != "https://api.parallel.ai/v1/search" || key != "synthetic-tool-key" || header != "x-api-key" || params["search_queries"] == nil || params["max_results"] != nil {
			t.Fatal("wrong Parallel protocol")
		}
		return map[string]interface{}{"results": []interface{}{
			map[string]interface{}{"url": "javascript:alert(1)", "title": "not allowed"},
			map[string]interface{}{"url": "https://docs.docker.com/compose/", "title": "Official docs", "excerpts": []string{"Contains synthetic-tool-key"}},
		}}, nil
	}
	result, err := CloudSearch(context.Background(), "Docker Compose networks", "")
	if err != nil || result.Provider != "parallel" || len(result.Results) != 1 || strings.Contains(result.Results[0].Excerpt, "synthetic-tool-key") {
		t.Fatalf("search normalization or secret redaction failed: %v", err)
	}
	if _, err := CloudSearch(context.Background(), "query synthetic-tool-key", ""); err == nil {
		t.Fatal("pasted credential sent to a search service")
	}
}

func TestCloudVisionUsesRemoteAPIAndRejectsURLFetches(t *testing.T) {
	isolateCloudToolTest(t)
	t.Setenv("OLLAMA_API_KEY", "synthetic-vision-key")
	t.Setenv("OLLAMA_API_KEYS", "")
	cloudTestConfig(t, CloudConfig{VisionOrder: []string{"ollama-cloud"}})
	var picture bytes.Buffer
	png.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 8, 8)))
	calls := 0
	cloudToolRequest = func(_ context.Context, method, endpoint, key, header string, body interface{}) (map[string]interface{}, error) {
		calls++
		if endpoint != "https://ollama.com/v1/chat/completions" || method != "POST" || key != "synthetic-vision-key" || header != "Authorization" {
			t.Fatal("cloud vision selected a local or incorrect endpoint")
		}
		return map[string]interface{}{"choices": []interface{}{map[string]interface{}{"message": map[string]interface{}{"content": "red"}}}}, nil
	}
	result, err := CloudVision(context.Background(), "data:image/png;base64,"+base64.StdEncoding.EncodeToString(picture.Bytes()), "Which color?", "")
	if err != nil || result.Provider != "ollama-cloud" || result.Answer != "red" || calls != 1 {
		t.Fatalf("cloud vision failed: %v", err)
	}
	for _, data := range []string{"https://internal.example/private.png", "data:image/png;base64,bm90LWltYWdl"} {
		if _, err := CloudVision(context.Background(), data, "Which color?", ""); err == nil {
			t.Fatal("remote fetch or invalid image accepted")
		}
	}
	if calls != 1 {
		t.Fatal("invalid image reached upstream")
	}
}
