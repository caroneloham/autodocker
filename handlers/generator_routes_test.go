package handlers

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"autodocker-go/db"
	"github.com/gin-gonic/gin"
)

func TestGeneratorRoutesDoNotExposeRemovedHostManagement(t *testing.T) {
	r := gin.New()
	RegisterGeneratorRoutes(r.Group("/api"))
	paths := map[string]bool{}
	for _, route := range r.Routes() {
		paths[route.Path] = true
	}
	for _, path := range []string{"/api/deploy", "/api/deploy/preview", "/api/ai-generate", "/api/ai-jobs", "/api/ai-jobs/:id", "/api/ai-jobs/:id/cancel", "/api/project/:name/export", "/api/docker/status"} {
		if !paths[path] {
			t.Fatal("missing generator route " + path)
		}
	}
	for _, path := range []string{"/api/ai-capabilities", "/api/ai-tools/search", "/api/ai-tools/vision"} {
		if !paths[path] {
			t.Fatal("missing authenticated cloud route " + path)
		}
	}
	for _, route := range []string{"/api/generate", "/api/generate/preview"} {
		if !paths[route] {
			t.Fatal("missing stateless generation route " + route)
		}
	}
	if len(paths) != 16 {
		t.Fatalf("unexpected generator API surface: %v", paths)
	}
}

func TestProjectDownloadsArePrivateAndIncludeClientDelivery(t *testing.T) {
	r, root := builderTestRouter(t, func(context.Context, string, ...string) (string, error) { return "", nil })
	r.GET("/api/project/:name/compose", handleProjectCompose)
	r.POST("/api/project/:name/export", handleProjectExport)
	for _, project := range []string{"alice-project", "bob-project"} {
		dir := filepath.Join(root, project)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"compose.yaml", "client.html", "supervision.py", "supervision.json", "GUIDE-CLIENT.md"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("private "+name), 0600); err != nil {
				t.Fatal(err)
			}
		}
		owner := "alice"
		if project == "bob-project" {
			owner = "bob"
		}
		if _, err := db.DB.Exec("INSERT INTO labs(project,owner) VALUES(?,?)", project, owner); err != nil {
			t.Fatal(err)
		}
	}
	for _, project := range []string{"alice-project", "bob-project"} {
		for _, suffix := range []string{"compose", "export"} {
			method := http.MethodGet
			if suffix == "export" {
				method = http.MethodPost
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(method, "/api/project/"+project+"/"+suffix, nil))
			if project == "bob-project" {
				if w.Code != 403 || strings.Contains(w.Body.String(), "private compose") {
					t.Fatal("other user's project was disclosed")
				}
				continue
			}
			if w.Code != 200 {
				t.Fatalf("own download returned %d", w.Code)
			}
			if suffix == "export" {
				archive, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
				if err != nil {
					t.Fatal(err)
				}
				if len(archive.File) != 5 {
					t.Fatal("delivery files missing from archive")
				}
			}
		}
	}
}
