package handlers

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"autodocker-go/db"
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
)

func TestGenerationNamesReusableWithoutDatabaseOrProjectWrites(t *testing.T) {
	r, root := builderTestRouter(t, nil)
	r.POST("/generate", handleGenerationExport)
	r.POST("/preview", handleGenerationPreview)
	oldDir := filepath.Join(root, "mon-site")
	if err := os.MkdirAll(oldDir, 0700); err != nil {
		t.Fatal(err)
	}
	sentinel := []byte("existing unrelated project must remain intact")
	if err := os.WriteFile(filepath.Join(oldDir, "compose.yaml"), sentinel, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec("INSERT INTO labs(project,owner) VALUES('mon-site','bob')"); err != nil {
		t.Fatal(err)
	}
	// Exports must work even when the historical project database is unavailable.
	if err := db.DB.Close(); err != nil {
		t.Fatal(err)
	}
	body := `{"project":"mon-site","payload":{"services":[{"key":"nginx","network":"app"}],"addressing":{"networks":[{"name":"app","cidr":"172.25.0.0/24"}]}}}`
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, route := range []string{"/preview", "/generate", "/generate"} {
				request := httptest.NewRequest("POST", route, strings.NewReader(body))
				request.Header.Set("Content-Type", "application/json")
				recorder := httptest.NewRecorder()
				r.ServeHTTP(recorder, request)
				if recorder.Code != 200 {
					t.Errorf("%s status=%d", route, recorder.Code)
					continue
				}
				if recorder.Header().Get("Cache-Control") != "no-store" {
					t.Error("private result must not be cached")
				}
				var result struct {
					Archive   string `json:"archive_base64"`
					Compose   string `json:"compose"`
					Retention string `json:"retention"`
					New       bool   `json:"is_new"`
				}
				if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
					t.Error(err)
					continue
				}
				if route == "/preview" {
					if !result.New {
						t.Error("saved names must not block preview")
					}
					continue
				}
				if result.Retention != "none" {
					t.Error("unexpected retained project")
				}
				data, err := base64.StdEncoding.DecodeString(result.Archive)
				if err != nil {
					t.Error(err)
					continue
				}
				archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
				if err != nil {
					t.Error(err)
					continue
				}
				found := map[string]bool{}
				for _, file := range archive.File {
					found[file.Name] = true
					if file.Name == "compose.yaml" {
						reader, _ := file.Open()
						raw, _ := io.ReadAll(reader)
						reader.Close()
						if string(raw) != result.Compose {
							t.Error("Compose download differs from ZIP")
						}
					}
				}
				for _, file := range []string{"compose.yaml", "up.sh", "down.sh", "GUIDE-AUTODOCKER.md"} {
					if !found[file] {
						t.Errorf("archive missing %s", file)
					}
				}
			}
		}()
	}
	wg.Wait()
	saved, err := os.ReadFile(filepath.Join(oldDir, "compose.yaml"))
	if err != nil || !bytes.Equal(saved, sentinel) {
		t.Fatal("existing project changed")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 {
		t.Fatal("export created project directories")
	}
}

func TestGenerationRejectsStartingContainersAndAnonymousRequests(t *testing.T) {
	r, _ := builderTestRouter(t, nil)
	r.POST("/generate", handleGenerationExport)
	recorder := httptest.NewRecorder()
	r.ServeHTTP(recorder, httptest.NewRequest("POST", "/generate", strings.NewReader(`{"project":"demo","deploy":true,"payload":{"services":[{"key":"nginx"}]}}`)))
	if recorder.Code != 400 {
		t.Fatalf("container start accepted: %d", recorder.Code)
	}
	anonymous := gin.New()
	anonymous.Use(sessions.Sessions("test", cookie.NewStore([]byte("anonymous-generation-fixture-key!"))))
	anonymous.POST("/generate", handleGenerationExport)
	recorder = httptest.NewRecorder()
	anonymous.ServeHTTP(recorder, httptest.NewRequest("POST", "/generate", strings.NewReader(`{}`)))
	if recorder.Code != 401 {
		t.Fatalf("anonymous export accepted: %d", recorder.Code)
	}
}

func TestGenerationArchiveRejectsUnsafeEntriesAndOversizedInput(t *testing.T) {
	for _, name := range []string{"../private.env", "/root/file", "C:/secret", "a/../../secret", "a\\secret"} {
		if _, err := generationArchive(map[string]string{name: "private"}); err == nil {
			t.Errorf("unsafe ZIP entry accepted: %s", name)
		}
	}
	if _, err := generationArchive(map[string]string{"large.txt": strings.Repeat("x", (16<<20)+1)}); err == nil {
		t.Fatal("unbounded archive accepted")
	}
}
