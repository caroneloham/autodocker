package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"autodocker-go/config"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
)

type dashboardStateDocker struct {
	containers []types.Container
	err        error
	calls      int
	all        bool
	hasTimeout bool
}

func (f *dashboardStateDocker) ContainerList(ctx context.Context, opts container.ListOptions) ([]types.Container, error) {
	f.calls++
	f.all = opts.All
	_, f.hasTimeout = ctx.Deadline()
	return f.containers, f.err
}

func dashboardStateRequest(t *testing.T, projectsDir string, docker dockerContainerLister) *httptest.ResponseRecorder {
	t.Helper()
	previousDir := config.ProjectsDir
	config.ProjectsDir = projectsDir
	t.Cleanup(func() { config.ProjectsDir = previousDir })
	router := gin.New()
	router.Use(sessions.Sessions("dashboard-test", cookie.NewStore([]byte("dashboard-test-key-only"))))
	router.GET("/api/refresh", func(c *gin.Context) {
		session := sessions.Default(c)
		session.Set("user", "test-admin")
		session.Set("role", "admin")
		handleRefreshWithDocker(c, docker)
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/refresh", nil))
	return recorder
}

func TestDashboardRefreshReportsRuntimeState(t *testing.T) {
	cases := []struct {
		name        string
		states      []string
		unavailable bool
		listError   bool
		wantState   string
		wantLegacy  string
	}{
		{name: "prepared with Docker available", wantState: "prepared", wantLegacy: "inactive"},
		{name: "stopped", states: []string{"exited", "created"}, wantState: "stopped", wantLegacy: "inactive"},
		{name: "running", states: []string{"running", "running"}, wantState: "running", wantLegacy: "active"},
		{name: "partial", states: []string{"running", "exited"}, wantState: "partial", wantLegacy: "active"},
		{name: "Docker unavailable", unavailable: true, wantState: "docker_unavailable", wantLegacy: "inactive"},
		{name: "Docker failed after connecting", states: []string{"running"}, listError: true, wantState: "docker_unavailable", wantLegacy: "inactive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			projectDir := filepath.Join(root, "Demo")
			if err := os.Mkdir(projectDir, 0755); err != nil {
				t.Fatal(err)
			}
			writeCompose(t, projectDir, "services:\n  web:\n    image: nginx:alpine\n")
			modified := time.Date(2026, time.June, 29, 20, 14, 0, 0, time.UTC)
			if err := os.Chtimes(filepath.Join(projectDir, "compose.yaml"), modified, modified); err != nil {
				t.Fatal(err)
			}
			fake := &dashboardStateDocker{}
			for i, state := range tc.states {
				fake.containers = append(fake.containers, types.Container{
					ID: string(rune('a' + i)), State: state,
					Labels: map[string]string{"com.docker.compose.project": "demo"},
				})
			}
			if tc.listError {
				fake.err = errors.New("Docker disconnected")
			}
			var docker dockerContainerLister = fake
			if tc.unavailable {
				docker = nil
			}
			recorder := dashboardStateRequest(t, root, docker)
			if recorder.Code != http.StatusOK {
				t.Fatalf("HTTP status = %d, want 200", recorder.Code)
			}
			var projects []projectInfo
			if err := json.Unmarshal(recorder.Body.Bytes(), &projects); err != nil {
				t.Fatalf("response must remain a JSON array: %v", err)
			}
			if len(projects) != 1 {
				t.Fatalf("projects = %d, want 1", len(projects))
			}
			project := projects[0]
			if project.RuntimeState != tc.wantState || project.Status != tc.wantLegacy {
				t.Errorf("runtime_state/status = %s/%s, want %s/%s", project.RuntimeState, project.Status, tc.wantState, tc.wantLegacy)
			}
			wantAvailable := !tc.unavailable && !tc.listError
			if project.DockerAvailable != wantAvailable {
				t.Errorf("docker_available = %v, want %v", project.DockerAvailable, wantAvailable)
			}
			if project.Name != "Demo" || project.Role != "write" || len(project.Services) != 1 {
				t.Errorf("existing metadata changed: %+v", project)
			}
			if project.ModifiedAt != modified.Format(time.RFC3339Nano) {
				t.Errorf("modified_at = %q, want Compose modification time %s", project.ModifiedAt, modified)
			}
			if !tc.unavailable && (fake.calls != 1 || !fake.all || !fake.hasTimeout) {
				t.Errorf("expected one bounded all-container query: %+v", fake)
			}
			if !wantAvailable && len(project.Containers) != 0 {
				t.Error("a failed Docker response must not be presented as live containers")
			}
		})
	}
}

func TestDashboardRefreshEmptyArrayStillReportsDockerAvailability(t *testing.T) {
	for _, available := range []bool{true, false} {
		t.Run(map[bool]string{true: "available", false: "unavailable"}[available], func(t *testing.T) {
			var docker dockerContainerLister
			if available {
				docker = &dashboardStateDocker{}
			}
			recorder := dashboardStateRequest(t, t.TempDir(), docker)
			if recorder.Body.String() != "[]" {
				t.Fatalf("empty dashboard must remain [], got %s", recorder.Body.String())
			}
			wantHeader := map[bool]string{true: "true", false: "false"}[available]
			if got := recorder.Header().Get("X-AutoDocker-Docker-Available"); got != wantHeader {
				t.Errorf("Docker availability header = %q, want %q", got, wantHeader)
			}
		})
	}
}

func TestDashboardRefreshDockerOnlyProjectKeepsContainerContract(t *testing.T) {
	fake := &dashboardStateDocker{containers: []types.Container{
		{ID: "unlabelled", State: "running"},
		{
			ID: "1234567890123456", Names: []string{"/demo-web-1"},
			State: "running", Image: "nginx:alpine",
			Labels: map[string]string{"com.docker.compose.project": "demo"},
			Ports: []types.Port{
				{PublicPort: 8080, PrivatePort: 80},
				{PublicPort: 8080, PrivatePort: 80},
			},
		},
	}}
	recorder := dashboardStateRequest(t, t.TempDir(), fake)
	var projects []map[string]interface{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &projects); err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 {
		t.Fatalf("expected one Compose project, got %d", len(projects))
	}
	project := projects[0]
	if project["runtime_state"] != "running" || project["status"] != "active" {
		t.Fatalf("unexpected Docker-only project state: %+v", project)
	}
	if _, exists := project["modified_at"]; exists {
		t.Error("Docker-only project must not invent a Compose modification date")
	}
	containers := project["containers"].([]interface{})
	ctr := containers[0].(map[string]interface{})
	if ctr["id"] != "123456789012" || ctr["full_id"] != "1234567890123456" || ctr["name"] != "demo-web-1" || ctr["ports"] != "8080->80" {
		t.Errorf("existing container fields changed: %+v", ctr)
	}
	if fake.calls != 1 {
		t.Errorf("ContainerList calls = %d, want 1", fake.calls)
	}
}
