package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"autodocker-go/config"
	"autodocker-go/db"
	"autodocker-go/middleware"
	"autodocker-go/services"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
)

func TestBackupVolumeOwnershipUsesLabelsNotOverlappingNames(t *testing.T) {
	for _, tc := range []struct {
		name, label         string
		privileged, allowed bool
	}{
		{"foo_data", "foo", false, true},
		{"foo_bar_data", "foo_bar", false, false},
		{"foo_bar_data", "foo_bar", true, false},
		{"custom_named_data", "foo", false, true},
		{"foo_external", "", false, false},
		{"foo_external", "", true, true},
		{"other_external", "", true, false},
	} {
		info := volume.Volume{Name: tc.name, Labels: map[string]string{"com.docker.compose.project": tc.label}}
		if got := backupVolumeAllowed(info, "foo", tc.privileged); got != tc.allowed {
			t.Errorf("name=%q label=%q privileged=%v allowed=%v", tc.name, tc.label, tc.privileged, got)
		}
	}
}

func TestBackupAndRestoreRejectCollidingProjectBeforeMountingVolume(t *testing.T) {
	r, loggedIn := accountTestRouter(t)
	if _, err := db.DB.Exec(`
		CREATE TABLE labs_acl(project TEXT, username TEXT, role TEXT);
		CREATE TABLE labs(project TEXT, owner TEXT);
		CREATE TABLE groups_acl(group_name TEXT, project TEXT, role TEXT);
		CREATE TABLE user_groups(group_name TEXT, username TEXT);
		CREATE TABLE backup_history(id INTEGER PRIMARY KEY, project TEXT, volume_name TEXT, filename TEXT);
		INSERT INTO labs(project,owner) VALUES('foo','alice'),('foo_bar','bob');
		INSERT INTO labs_acl(project,username,role) VALUES('foo','owner','write');
		INSERT INTO backup_history VALUES(1,'foo','foo_bar_data','old.tar.gz');
	`); err != nil {
		t.Fatal(err)
	}
	oldBackups := config.BackupsDir
	config.BackupsDir = t.TempDir()
	t.Cleanup(func() { config.BackupsDir = oldBackups })
	volumes := []volume.Volume{
		{Name: "foo_bar_data", Labels: map[string]string{"com.docker.compose.project": "foo_bar"}},
		{Name: "foo_data", Labels: map[string]string{"com.docker.compose.project": "foo"}},
		{Name: "custom_named_data", Labels: map[string]string{"com.docker.compose.project": "foo"}},
		{Name: "foo_external"},
	}
	mountAttempts := 0
	dockerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := strings.TrimPrefix(req.URL.Path, "/v1.47")
		switch {
		case path == "/_ping":
			w.Write([]byte("OK"))
		case path == "/volumes":
			json.NewEncoder(w).Encode(map[string]interface{}{"Volumes": volumes, "Warnings": []string{}})
		case strings.HasPrefix(path, "/volumes/"):
			for _, info := range volumes {
				if info.Name == strings.TrimPrefix(path, "/volumes/") {
					json.NewEncoder(w).Encode(info)
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
		case path == "/containers/create":
			mountAttempts++
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Errorf("unexpected Docker API call %s %s", req.Method, req.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(dockerServer.Close)
	dockerClient, err := client.NewClientWithOpts(client.WithHost(dockerServer.URL), client.WithVersion("1.47"))
	if err != nil {
		t.Fatal(err)
	}
	oldDocker := services.DockerClient
	services.DockerClient = dockerClient
	t.Cleanup(func() { services.DockerClient = oldDocker; dockerClient.Close() })
	RegisterBackupRoutes(r.Group("/api", middleware.LoginRequired()))
	alice := loggedIn("alice")
	for _, request := range []struct{ path, body string }{
		{"/api/backups/create", `{"project":"foo","volume_name":"foo_bar_data"}`},
		{"/api/backups/create", `{"project":"foo","volume_name":"foo_external"}`},
		{"/api/backups/restore", `{"backup_id":1}`},
	} {
		if response := alice.request(http.MethodPost, request.path, request.body); response.Code != http.StatusForbidden {
			t.Fatalf("%s: want 403, got %d %s", request.path, response.Code, response.Body.String())
		}
	}
	if mountAttempts != 0 {
		t.Fatal("unauthorized volume reached ContainerCreate")
	}
	response := alice.request(http.MethodGet, "/api/backups/volumes?project=foo", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "foo_data") || !strings.Contains(response.Body.String(), "custom_named_data") || strings.Contains(response.Body.String(), "foo_bar_data") || strings.Contains(response.Body.String(), "foo_external") {
		t.Fatalf("volume list crossed project boundaries: %d %s", response.Code, response.Body.String())
	}
	for _, username := range []string{"owner", "legacy-admin"} {
		response := loggedIn(username).request(http.MethodGet, "/api/backups/volumes?project=foo", "")
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "foo_external") || strings.Contains(response.Body.String(), "foo_bar_data") {
			t.Fatalf("%s lost explicit access to legacy volumes: %d %s", username, response.Code, response.Body.String())
		}
	}
}
