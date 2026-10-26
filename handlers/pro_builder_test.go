package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"autodocker-go/services"
	"github.com/goccy/go-yaml"
)

func TestProPreviewMetadataAndSavedRoutesMatchWithoutStartingContainers(t *testing.T) {
	for _, proxy := range []string{"caddy", "traefik", "haproxy"} {
		t.Run(proxy, func(t *testing.T) {
			calls := 0
			r, root := builderTestRouter(t, func(_ context.Context, _ string, args ...string) (string, error) {
				calls++
				if len(args) < 2 || strings.Join(args[len(args)-2:], " ") != "config --quiet" {
					t.Fatalf("unexpected container operation: %v", args)
				}
				return "", nil
			})
			image := map[string]string{"caddy": "caddy:2-alpine", "haproxy": "haproxy:3.0-alpine"}[proxy]
			payload := services.BuildComposePayload{Ingress: &services.IngressConfig{Proxy: proxy, Domain: "localhost", Port: 18088}, Services: []services.ServiceEntry{{Key: proxy, Image: image, Network: "app"}, {Key: "nginx", Network: "app"}}, Addressing: services.AddressingConfig{Networks: []services.NetworkDef{{Name: "app", CIDR: "172.29.10.0/24", Gateway: "172.29.10.1"}}}}
			body, _ := json.Marshal(map[string]interface{}{"project": "pro-test", "payload": payload, "deploy": false})
			request := func(path string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				r.ServeHTTP(w, req)
				if w.Code != http.StatusOK {
					t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
				}
				return w
			}
			w := request("/api/deploy/preview")
			var preview struct {
				YAML    string `json:"new_yaml"`
				Ingress struct {
					Ingress struct {
						Proxy  string        `json:"proxy"`
						Routes []interface{} `json:"routes"`
					} `json:"ingress"`
				} `json:"ingress"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &preview); err != nil {
				t.Fatal(err)
			}
			if preview.Ingress.Ingress.Proxy != proxy || len(preview.Ingress.Ingress.Routes) != 1 {
				t.Fatalf("missing actual routing metadata: %s", w.Body.String())
			}
			if _, err := os.Stat(filepath.Join(root, "pro-test")); !os.IsNotExist(err) {
				t.Fatal("preview created a project")
			}
			request("/api/deploy")
			path := filepath.Join(root, "pro-test", "compose.yaml")
			saved, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(saved) != preview.YAML {
				t.Fatal("saved routing differs from preview")
			}
			var config services.M
			if err := yaml.Unmarshal(saved, &config); err != nil {
				t.Fatal(err)
			}
			// Parse the final generated artifact with the installed Compose CLI, without a daemon.
			if dockerComposeAvailable() {
				cmd := exec.Command("docker", "compose", "-f", path, "config", "--quiet")
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("invalid Compose: %s %v", out, err)
				}
			}
			if calls != 1 {
				t.Fatalf("%d operations; expected config validation only", calls)
			}
		})
	}
}

func TestProInvalidRoutingRejectedByBothEndpointsBeforeFilesAreWritten(t *testing.T) {
	r, root := builderTestRouter(t, func(context.Context, string, ...string) (string, error) {
		t.Fatal("invalid routing reached execution")
		return "", nil
	})
	for _, endpoint := range []string{"/api/deploy/preview", "/api/deploy"} {
		body := `{"project":"bad-pro","payload":{"services":[{"key":"caddy","image":"caddy:2-alpine"}],"ingress":{"proxy":"caddy","domain":"localhost;echo bad","port":8088}}}`
		req := httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s accepted unsafe settings: %s", endpoint, w.Body.String())
		}
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatal("invalid request created files")
	}
}
