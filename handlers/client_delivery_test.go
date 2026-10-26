package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClientDeliveryPreviewSaveAndPreserveClientCustomizations(t *testing.T) {
	calls := 0
	router, root := builderTestRouter(t, func(context.Context, string, ...string) (string, error) { calls++; return "", nil })
	body := `{"project":"client-handoff","deploy":false,"payload":{"services":[{"key":"nginx","network":"app"}],"client":{"name":"Client test","accent":"#4f8cff"},"supervision":{"gpu":false,"disk_path":"/srv","disk_warning":85,"min_free_gib":15,"temperature_warning":85,"backup_max_hours":30}}}`
	request := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		return w
	}
	preview := request("/api/deploy/preview")
	var response struct {
		Metadata struct {
			Delivery struct {
				Version int `json:"version"`
			} `json:"client_delivery"`
		} `json:"ingress"`
	}
	if err := json.Unmarshal(preview.Body.Bytes(), &response); err != nil || response.Metadata.Delivery.Version != 1 {
		t.Fatal("preview omitted delivery metadata")
	}
	if _, err := os.Stat(filepath.Join(root, "client-handoff")); !os.IsNotExist(err) {
		t.Fatal("preview wrote project files")
	}
	request("/api/deploy")
	for _, name := range []string{"client.html", "client.json", "supervision.py", "supervision.json", "GUIDE-CLIENT.md"} {
		if data, err := os.ReadFile(filepath.Join(root, "client-handoff", name)); err != nil || len(data) == 0 {
			t.Fatalf("missing %s", name)
		}
	}
	custom := filepath.Join(root, "client-handoff", "supervision.json")
	if err := os.WriteFile(custom, []byte(`{"adapted":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	request("/api/deploy")
	data, _ := os.ReadFile(custom)
	if string(data) != `{"adapted":true}` {
		t.Fatal("overwrote per-client adaptation")
	}
	if calls != 2 {
		t.Fatal("unexpected Docker operations")
	}
}

func TestClientDeliveryInvalidSettingsRejectedBeforeSaving(t *testing.T) {
	router, root := builderTestRouter(t, func(context.Context, string, ...string) (string, error) {
		t.Fatal("invalid delivery reached Docker")
		return "", nil
	})
	body := `{"project":"invalid-delivery","payload":{"services":[{"key":"nginx"}],"client":{"name":"Client","accent":"url(bad)"}}}`
	for _, path := range []string{"/api/deploy/preview", "/api/deploy"} {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatalf("%s: unsafe config returned %d", path, w.Code)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "invalid-delivery")); !os.IsNotExist(err) {
		t.Fatal("invalid request wrote files")
	}
}
