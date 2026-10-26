package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"autodocker-go/config"
	"autodocker-go/db"
	"autodocker-go/services"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/goccy/go-yaml"
)

func builderTestRouter(t *testing.T, runner composeCommandRunner) (*gin.Engine, string) {
	t.Helper()
	oldDB, oldProjectsDir := db.DB, config.ProjectsDir
	testDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	testDB.SetMaxOpenConns(1)
	db.DB, config.ProjectsDir = testDB, t.TempDir()
	t.Cleanup(func() { testDB.Close(); db.DB, config.ProjectsDir = oldDB, oldProjectsDir })
	for _, query := range []string{
		"CREATE TABLE labs (project TEXT PRIMARY KEY, owner TEXT)",
		"CREATE TABLE labs_acl (project TEXT, username TEXT, role TEXT, UNIQUE(project, username))",
		"CREATE TABLE groups_acl (project TEXT, group_name TEXT, role TEXT)",
		"CREATE TABLE user_groups (username TEXT, group_name TEXT)",
		"CREATE TABLE activity_log (date TEXT, username TEXT, action TEXT, target TEXT, details TEXT)",
	} {
		if _, err := testDB.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	r := gin.New()
	r.Use(sessions.Sessions("test", cookie.NewStore([]byte("builder-test-session-key-32bytes!"))))
	r.Use(func(c *gin.Context) {
		session := sessions.Default(c)
		session.Set("user", "alice")
		session.Set("role", "user")
		c.Next()
	})
	noOccupiedSubnets := func(string, string) map[string]bool { return map[string]bool{} }
	r.POST("/api/deploy", func(c *gin.Context) { handleDeployUsing(c, runner, noOccupiedSubnets) })
	r.POST("/api/deploy/preview", func(c *gin.Context) { handleDeployPreviewUsing(c, noOccupiedSubnets) })
	return r, config.ProjectsDir
}

func TestPrepareBuilderPayloadKeepsNetworkAndAddressesAligned(t *testing.T) {
	payload := services.BuildComposePayload{
		Addressing: services.AddressingConfig{Networks: []services.NetworkDef{{Name: "front", CIDR: "172.20.10.0/24", Gateway: "172.20.10.99"}}},
		Services: []services.ServiceEntry{
			{Key: "nginx", Network: "front", IP: "172.20.10.20"},
			{Key: "debian-router", LAN: &services.RouterInterface{Network: "front", IP: "172.20.10.30"}},
		},
	}
	prepared := prepareBuilderPayload(payload, map[string]bool{"172.20.10.0/24": true})
	if prepared.Addressing.Networks[0].CIDR != "172.20.11.0/24" || prepared.Addressing.Networks[0].Gateway != "172.20.11.1" {
		t.Fatalf("reseau mal remappe: %#v", prepared.Addressing.Networks[0])
	}
	if prepared.Services[0].IP != "172.20.11.20" || prepared.Services[1].LAN.IP != "172.20.11.30" {
		t.Fatalf("adresses incoherentes: %#v %#v", prepared.Services[0], prepared.Services[1].LAN)
	}
	if payload.Addressing.Networks[0].CIDR != "172.20.10.0/24" || payload.Services[1].LAN.IP != "172.20.10.30" {
		t.Fatal("la preparation a modifie le payload original")
	}
	unchanged := prepareBuilderPayload(payload, map[string]bool{})
	if unchanged.Addressing.Networks[0].CIDR != "172.20.10.0/24" || unchanged.Services[0].IP != "172.20.10.20" {
		t.Fatal("un projet doit conserver son reseau non occupe")
	}
}

func TestPreviewAndSavedComposeUseSameNetworkIPAndPorts(t *testing.T) {
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	busyPort := listener.Addr().(*net.TCPAddr).Port
	r, root := builderTestRouter(t, func(context.Context, string, ...string) (string, error) { return "", nil })
	occupied := func(string, string) map[string]bool { return map[string]bool{"172.20.10.0/24": true} }
	r.POST("/preview-network", func(c *gin.Context) { handleDeployPreviewUsing(c, occupied) })
	r.POST("/prepare-network", func(c *gin.Context) {
		handleDeployUsing(c, func(context.Context, string, ...string) (string, error) { return "", nil }, occupied)
	})
	body, err := json.Marshal(map[string]interface{}{
		"project": "demo", "deploy": false,
		"payload": services.BuildComposePayload{
			Addressing: services.AddressingConfig{Networks: []services.NetworkDef{{Name: "front", CIDR: "172.20.10.0/24"}}},
			Services:   []services.ServiceEntry{{Key: "web-test", Image: "nginx:alpine", Network: "front", IP: "172.20.10.20", Ports: []string{fmt.Sprintf("%d:80", busyPort)}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := func(endpoint string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", endpoint, w.Code, w.Body.String())
		}
		return w
	}
	preview := request("/preview-network")
	var result struct {
		NewYAML string `json:"new_yaml"`
	}
	if err := json.Unmarshal(preview.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	request("/prepare-network")
	saved, err := os.ReadFile(filepath.Join(root, "demo", "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(saved) != result.NewYAML {
		t.Fatalf("compose sauvegarde different de l'apercu\npreview=%s\nsaved=%s", result.NewYAML, saved)
	}
	var parsed services.M
	if err := yaml.Unmarshal(saved, &parsed); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), "172.20.11.0/24") || !strings.Contains(string(saved), "172.20.11.20") || !strings.Contains(string(saved), "172.20.11.1") {
		t.Fatalf("reseau, IP ou passerelle incoherents: %s", saved)
	}
	if strings.Contains(string(saved), fmt.Sprintf("%d:80", busyPort)) {
		t.Fatal("le port occupe a ete reutilise")
	}
}

func TestLocalBuilderPreviewAndSavedComposeProtectInference(t *testing.T) {
	r, root := builderTestRouter(t, func(context.Context, string, ...string) (string, error) { return "", nil })
	body, err := json.Marshal(map[string]interface{}{
		"project": "private-ai", "deploy": false,
		"payload": services.BuildComposePayload{Services: []services.ServiceEntry{{Key: "ollama"}, {Key: "open-webui"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	check := func(data []byte) {
		t.Helper()
		var compose struct {
			Services struct {
				Ollama struct {
					Ports []string `yaml:"ports"`
				} `yaml:"ollama"`
				OpenWebUI struct {
					Ports       []string          `yaml:"ports"`
					Environment map[string]string `yaml:"environment"`
				} `yaml:"open-webui"`
			} `yaml:"services"`
		}
		if err := yaml.Unmarshal(data, &compose); err != nil {
			t.Fatal(err)
		}
		if len(compose.Services.Ollama.Ports) != 0 {
			t.Fatalf("Ollama host port exposed: %v", compose.Services.Ollama.Ports)
		}
		web := compose.Services.OpenWebUI
		if len(web.Ports) == 0 || !strings.HasPrefix(web.Ports[0], "127.0.0.1:") {
			t.Fatalf("Open WebUI is not limited to localhost: %v", web.Ports)
		}
		if web.Environment["WEBUI_AUTH"] != "True" || web.Environment["ENABLE_SIGNUP"] != "False" || len(web.Environment["WEBUI_ADMIN_PASSWORD"]) < 24 {
			t.Fatal("Open WebUI authentication defaults are unsafe")
		}
	}
	for _, endpoint := range []string{"/api/deploy/preview", "/api/deploy"} {
		request := httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(string(body)))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		r.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", endpoint, response.Code, response.Body.String())
		}
		if endpoint == "/api/deploy/preview" {
			var preview struct {
				NewYAML string `json:"new_yaml"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &preview); err != nil {
				t.Fatal(err)
			}
			check([]byte(preview.NewYAML))
		} else {
			saved, err := os.ReadFile(filepath.Join(root, "private-ai", "compose.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			check(saved)
		}
	}
}

func builderRequest(t *testing.T, r http.Handler, endpoint, project string, deploy bool) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]interface{}{
		"project": project, "deploy": deploy,
		"payload": map[string]interface{}{"services": []map[string]string{{"key": "web-test", "image": "nginx:alpine"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestPrepareProjectPreservesExistingFilesAndContainers(t *testing.T) {
	var commands []string
	r, root := builderTestRouter(t, func(ctx context.Context, dir string, args ...string) (string, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("commande sans delai maximal")
		}
		commands = append(commands, strings.Join(args, " "))
		return "", nil
	})
	projDir := filepath.Join(root, "demo")
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeCompose(t, projDir, "services:\n  original:\n    image: nginx:stable\n")
	for _, name := range []string{"index.html", "up.sh", "auth.log"} {
		if err := os.WriteFile(filepath.Join(projDir, name), []byte("contenu utilisateur"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.DB.Exec("INSERT INTO labs VALUES ('demo', 'alice')"); err != nil {
		t.Fatal(err)
	}
	w := builderRequest(t, r, "/api/deploy", " DEMO ", false)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	for _, name := range []string{"index.html", "up.sh", "auth.log"} {
		data, err := os.ReadFile(filepath.Join(projDir, name))
		if err != nil || string(data) != "contenu utilisateur" {
			t.Fatalf("fichier %s modifie: %q %v", name, data, err)
		}
	}
	for _, cmd := range commands {
		if !strings.HasSuffix(cmd, "config --quiet") {
			t.Fatalf("preparation a pilote Docker: %s", cmd)
		}
	}
	versions, err := filepath.Glob(filepath.Join(projDir, "versions", "compose_*.yaml"))
	if err != nil || len(versions) != 1 {
		t.Fatalf("snapshot manquant: %v %v", versions, err)
	}
	old, err := os.ReadFile(versions[0])
	if err != nil || !strings.Contains(string(old), "original:") {
		t.Fatalf("snapshot incorrect: %q %v", old, err)
	}
}

func TestDeployValidationFailurePreservesPreviousProject(t *testing.T) {
	var calls int
	r, root := builderTestRouter(t, func(ctx context.Context, dir string, args ...string) (string, error) {
		calls++
		return "configuration refusee", errors.New("invalid compose")
	})
	projDir := filepath.Join(root, "demo")
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatal(err)
	}
	original := "services:\n  original:\n    image: nginx:stable\n"
	writeCompose(t, projDir, original)
	if _, err := db.DB.Exec("INSERT INTO labs VALUES ('demo', 'alice')"); err != nil {
		t.Fatal(err)
	}
	w := builderRequest(t, r, "/api/deploy", "demo", true)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	data, err := os.ReadFile(filepath.Join(projDir, "compose.yaml"))
	if err != nil || string(data) != original {
		t.Fatalf("ancien compose modifie: %q %v", data, err)
	}
	entries, err := os.ReadDir(projDir)
	if err != nil || len(entries) != 1 || calls != 1 {
		t.Fatalf("effets de bord: entries=%v calls=%d err=%v", entries, calls, err)
	}
}

func TestDeployStartsOnlyAfterValidation(t *testing.T) {
	var calls []string
	r, root := builderTestRouter(t, func(ctx context.Context, dir string, args ...string) (string, error) {
		cmd := strings.Join(args, " ")
		calls = append(calls, cmd)
		if strings.Contains(cmd, "up -d") {
			if _, err := os.Stat(filepath.Join(dir, "compose.yaml")); err != nil {
				t.Errorf("compose non sauvegarde: %v", err)
			}
		}
		return "", nil
	})
	w := builderRequest(t, r, "/api/deploy", "demo", true)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if len(calls) != 2 || !strings.HasSuffix(calls[0], "config --quiet") || !strings.HasSuffix(calls[1], "up -d --remove-orphans") {
		t.Fatalf("ordre commandes inattendu: %v", calls)
	}
	if _, err := os.Stat(filepath.Join(root, "demo", "compose.yaml")); err != nil {
		t.Fatal(err)
	}
}

func TestDeployAndPreviewRejectUnsafeNamesAndUnauthorizedProjects(t *testing.T) {
	r, _ := builderTestRouter(t, func(context.Context, string, ...string) (string, error) {
		t.Fatal("Docker ne doit pas etre appele")
		return "", nil
	})
	if _, err := db.DB.Exec("INSERT INTO labs VALUES ('private', 'bob')"); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"/api/deploy", "/api/deploy/preview"} {
		for _, tc := range []struct {
			name string
			code int
		}{
			{"../private", http.StatusBadRequest}, {"a/b", http.StatusBadRequest}, {"a.b", http.StatusBadRequest},
			{"private", http.StatusForbidden},
		} {
			t.Run(endpoint+tc.name, func(t *testing.T) {
				w := builderRequest(t, r, endpoint, tc.name, false)
				if w.Code != tc.code {
					t.Fatalf("status=%d attendu=%d body=%s", w.Code, tc.code, w.Body.String())
				}
			})
		}
	}
}

func TestPrepareFailureDoesNotRegisterProject(t *testing.T) {
	r, root := builderTestRouter(t, func(context.Context, string, ...string) (string, error) { return "", nil })
	projDir := filepath.Join(root, "demo")
	if err := os.MkdirAll(filepath.Join(projDir, "compose.yaml"), 0755); err != nil {
		t.Fatal(err)
	}
	w := builderRequest(t, r, "/api/deploy", "demo", false)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var count int
	if err := db.DB.QueryRow("SELECT COUNT(*) FROM labs").Scan(&count); err != nil || count != 0 {
		t.Fatalf("projet fantome en base: %d %v", count, err)
	}
}

func TestPrepareSnapshotFailureKeepsPreviousCompose(t *testing.T) {
	r, root := builderTestRouter(t, func(context.Context, string, ...string) (string, error) { return "", nil })
	projDir := filepath.Join(root, "demo")
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatal(err)
	}
	original := "services:\n  original:\n    image: nginx:stable\n"
	writeCompose(t, projDir, original)
	if err := os.WriteFile(filepath.Join(projDir, "versions"), []byte("fichier utilisateur"), 0644); err != nil {
		t.Fatal(err)
	}
	w := builderRequest(t, r, "/api/deploy", "demo", false)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	data, err := os.ReadFile(filepath.Join(projDir, "compose.yaml"))
	if err != nil || string(data) != original {
		t.Fatalf("ancien compose modifie: %q %v", data, err)
	}
	var count int
	if err := db.DB.QueryRow("SELECT COUNT(*) FROM labs").Scan(&count); err != nil || count != 0 {
		t.Fatalf("transaction non annulee: %d %v", count, err)
	}
}

func TestPrepareWorksWithoutDockerButDeployDoesNot(t *testing.T) {
	for _, deploy := range []bool{false, true} {
		t.Run(map[bool]string{false: "prepare", true: "deploy"}[deploy], func(t *testing.T) {
			r, root := builderTestRouter(t, func(context.Context, string, ...string) (string, error) {
				return "", &exec.Error{Name: "docker", Err: exec.ErrNotFound}
			})
			w := builderRequest(t, r, "/api/deploy", "demo", deploy)
			if deploy {
				if w.Code != http.StatusUnprocessableEntity {
					t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
				}
				if _, err := os.Stat(filepath.Join(root, "demo")); !os.IsNotExist(err) {
					t.Fatalf("dossier fantome apres echec: %v", err)
				}
			} else if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestDeployFailureKeepsPreparedProject(t *testing.T) {
	r, root := builderTestRouter(t, func(_ context.Context, _ string, args ...string) (string, error) {
		if args[0] == "up" {
			return "moteur indisponible", errors.New("docker unavailable")
		}
		return "", nil
	})
	w := builderRequest(t, r, "/api/deploy", "demo", true)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "demo", "compose.yaml")); err != nil {
		t.Fatalf("compose prepare perdu: %v", err)
	}
	var owner string
	if err := db.DB.QueryRow("SELECT owner FROM labs WHERE project='demo'").Scan(&owner); err != nil || owner != "alice" {
		t.Fatalf("projet non retrouvable: %q %v", owner, err)
	}
}

func TestDeployAndPreviewFailClosedOnDatabaseError(t *testing.T) {
	r, root := builderTestRouter(t, func(context.Context, string, ...string) (string, error) {
		t.Fatal("Docker ne doit pas etre appele apres erreur DB")
		return "", nil
	})
	if err := db.DB.Close(); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"/api/deploy", "/api/deploy/preview"} {
		w := builderRequest(t, r, endpoint, "demo", false)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("fichiers crees apres erreur DB: %v %v", entries, err)
	}
}

func dockerComposeAvailable() bool {
	if _, err := exec.LookPath("docker"); err != nil {
		return false
	}
	return exec.Command("docker", "compose", "version").Run() == nil
}

func writeCompose(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(content), 0644); err != nil {
		t.Fatalf("ecriture compose.yaml: %v", err)
	}
}

func TestValidateComposeProjectValid(t *testing.T) {
	if !dockerComposeAvailable() {
		t.Skip("docker compose indisponible sur cette machine")
	}
	dir := t.TempDir()
	writeCompose(t, dir, "services:\n  web:\n    image: nginx:alpine\n")

	out, err := validateComposeProject(dir)
	if err != nil {
		t.Fatalf("compose valide rejete: %v (sortie: %s)", err, out)
	}
	if out != "" {
		t.Fatalf("sortie attendue vide pour un compose valide, recu: %s", out)
	}
}

func TestValidateComposeProjectInvalid(t *testing.T) {
	if !dockerComposeAvailable() {
		t.Skip("docker compose indisponible sur cette machine")
	}
	dir := t.TempDir()
	// "image" must be a string; a mapping makes `docker compose config` fail.
	writeCompose(t, dir, "services:\n  web:\n    image:\n      not: a-string\n")

	out, err := validateComposeProject(dir)
	if err == nil {
		t.Fatal("compose invalide accepte, erreur attendue")
	}
	if out == "" {
		t.Fatal("sortie de docker compose config attendue pour diagnostiquer l'erreur")
	}
}

func TestValidateComposeProjectMissingFile(t *testing.T) {
	if !dockerComposeAvailable() {
		t.Skip("docker compose indisponible sur cette machine")
	}
	dir := t.TempDir()

	if _, err := validateComposeProject(dir); err == nil {
		t.Fatal("dossier sans compose.yaml accepte, erreur attendue")
	}
}
