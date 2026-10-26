package handlers

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"autodocker-go/config"
	"autodocker-go/db"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
)

type fakeLifecycleDocker struct {
	containers     []types.Container
	inspectErr     error
	listErr        error
	actionErrors   map[string]error
	calls          []string
	copied         bool
	inspectContext func(context.Context)
	copyContext    context.Context
	restartContext func(context.Context)
}

func (f *fakeLifecycleDocker) ContainerInspect(ctx context.Context, id string) (types.ContainerJSON, error) {
	if f.inspectContext != nil {
		f.inspectContext(ctx)
	}
	if f.inspectErr != nil {
		return types.ContainerJSON{}, f.inspectErr
	}
	return types.ContainerJSON{ContainerJSONBase: &types.ContainerJSONBase{ID: id, Name: "/demo-web"}, Config: &container.Config{Labels: map[string]string{"com.docker.compose.project": "demo"}}}, nil
}
func (f *fakeLifecycleDocker) ContainerList(context.Context, container.ListOptions) ([]types.Container, error) {
	return f.containers, f.listErr
}
func (f *fakeLifecycleDocker) action(ctx context.Context, id, action string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	f.calls = append(f.calls, action+":"+id)
	return f.actionErrors[action+":"+id]
}
func (f *fakeLifecycleDocker) ContainerStart(ctx context.Context, id string, _ container.StartOptions) error {
	return f.action(ctx, id, "start")
}
func (f *fakeLifecycleDocker) ContainerStop(ctx context.Context, id string, _ container.StopOptions) error {
	return f.action(ctx, id, "stop")
}
func (f *fakeLifecycleDocker) ContainerRestart(ctx context.Context, id string, _ container.StopOptions) error {
	if f.restartContext != nil {
		f.restartContext(ctx)
	}
	return f.action(ctx, id, "restart")
}
func (f *fakeLifecycleDocker) ContainerRemove(ctx context.Context, id string, _ container.RemoveOptions) error {
	return f.action(ctx, id, "remove")
}
func (f *fakeLifecycleDocker) CopyToContainer(ctx context.Context, _ string, _ string, content io.Reader, _ container.CopyToContainerOptions) error {
	f.copyContext = ctx
	if ctx.Err() != nil {
		return ctx.Err()
	}
	_, err := io.Copy(io.Discard, content)
	f.copied = err == nil
	return err
}

func lifecycleRouter(t *testing.T) *gin.Engine {
	t.Helper()
	previousDB := db.DB
	previousProjectsDir := config.ProjectsDir
	config.ProjectsDir = t.TempDir()
	testDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.DB = testDB
	t.Cleanup(func() { testDB.Close(); db.DB = previousDB; config.ProjectsDir = previousProjectsDir })
	if _, err := testDB.Exec("CREATE TABLE activity_log(timestamp TEXT, username TEXT, action TEXT, target TEXT, details TEXT)"); err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	r.Use(sessions.Sessions("test", cookie.NewStore([]byte("lifecycle-test-session-key-32bytes"))))
	r.Use(func(c *gin.Context) {
		s := sessions.Default(c)
		s.Set("user", "test")
		s.Set("role", "super_admin")
		c.Next()
	})
	return r
}

func lifecycleRequest(r http.Handler, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestLifecycleInvalidActionDoesNotResolveDocker(t *testing.T) {
	r := lifecycleRouter(t)
	r.POST("/action", func(c *gin.Context) {
		handleContainerActionUsing(c, func() containerActionClient { t.Fatal("Docker appele pour une requete invalide"); return nil })
	})
	for _, body := range []string{`{"action":"start"}`, `{"id":"web","action":"invalid"}`, `{"id":"  ","action":"stop"}`} {
		w := lifecycleRequest(r, "/action", body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	}
}

func TestLifecycleInspectErrorsKeepTheirMeaning(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
	}{
		{"missing", errdefs.NotFound(errors.New("gone")), http.StatusNotFound},
		{"timeout", context.DeadlineExceeded, http.StatusGatewayTimeout},
		{"disconnected", errdefs.Unavailable(errors.New("Docker offline")), http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := lifecycleRouter(t)
			fake := &fakeLifecycleDocker{inspectErr: tc.err}
			r.POST("/action", func(c *gin.Context) { handleContainerActionUsing(c, func() containerActionClient { return fake }) })
			w := lifecycleRequest(r, "/action", `{"id":"web","action":"start"}`)
			if w.Code != tc.status || len(fake.calls) != 0 {
				t.Fatalf("status=%d want=%d calls=%v body=%s", w.Code, tc.status, fake.calls, w.Body.String())
			}
		})
	}
}

func TestLifecycleActionHonorsRequestCancellation(t *testing.T) {
	r := lifecycleRouter(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fake := &fakeLifecycleDocker{inspectContext: func(ctx context.Context) {
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Error("contexte de requete abandonne")
		}
	}}
	r.POST("/action", func(c *gin.Context) { handleContainerActionUsing(c, func() containerActionClient { return fake }) })
	req := httptest.NewRequest(http.MethodPost, "/action", strings.NewReader(`{"id":"web","action":"restart"}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if len(fake.calls) != 0 || w.Code == http.StatusOK {
		t.Fatalf("action executee apres annulation: %v status=%d", fake.calls, w.Code)
	}
}

func TestLifecycleFileSaveReportsRestartFailure(t *testing.T) {
	r := lifecycleRouter(t)
	fake := &fakeLifecycleDocker{actionErrors: map[string]error{"restart:web": errors.New("restart refused")}}
	r.POST("/file/:cid", func(c *gin.Context) { handleContainerFileWriteUsing(c, fake) })
	w := lifecycleRequest(r, "/file/web", `{"path":"/application/test.txt","content":"example"}`)
	if w.Code == http.StatusOK || !fake.copied || !strings.Contains(w.Body.String(), `"saved":true`) || !strings.Contains(w.Body.String(), `"restarted":false`) {
		t.Fatalf("faux succes apres redemarrage echoue: copied=%v status=%d body=%s", fake.copied, w.Code, w.Body.String())
	}
}

func TestLifecycleFileRestartHasIndependentTimeBudget(t *testing.T) {
	r := lifecycleRouter(t)
	fake := &fakeLifecycleDocker{}
	fake.restartContext = func(ctx context.Context) {
		if fake.copyContext == nil || !errors.Is(fake.copyContext.Err(), context.Canceled) {
			t.Error("le transfert termine doit liberer son contexte")
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) < 20*time.Second || time.Until(deadline) > 30*time.Second {
			t.Errorf("delai redemarrage incorrect: %v", deadline)
		}
		if ctx.Err() != nil {
			t.Errorf("redemarrage annule avec le transfert: %v", ctx.Err())
		}
	}
	r.POST("/file/:cid", func(c *gin.Context) { handleContainerFileWriteUsing(c, fake) })
	w := lifecycleRequest(r, "/file/web", `{"path":"/application/test.txt","content":"example"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"restarted":true`) {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestLifecycleSingleActionsPreserveSuccessContract(t *testing.T) {
	for _, action := range []string{"start", "stop", "restart", "remove"} {
		t.Run(action, func(t *testing.T) {
			r := lifecycleRouter(t)
			fake := &fakeLifecycleDocker{}
			r.POST("/action", func(c *gin.Context) { handleContainerActionUsing(c, func() containerActionClient { return fake }) })
			w := lifecycleRequest(r, "/action", `{"id":"web","action":"`+action+`"}`)
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"ok"`) || strings.Join(fake.calls, ",") != action+":web" {
				t.Fatalf("status=%d calls=%v body=%s", w.Code, fake.calls, w.Body.String())
			}
		})
	}
}

func TestLifecycleBulkNoMatchingProjectIsNotSuccess(t *testing.T) {
	r := lifecycleRouter(t)
	fake := &fakeLifecycleDocker{containers: []types.Container{{ID: "unrelated", State: "running", Labels: map[string]string{"com.docker.compose.project": "other"}}}}
	r.POST("/bulk", func(c *gin.Context) { handleBulkActionUsing(c, func() containerBulkClient { return fake }) })
	w := lifecycleRequest(r, "/bulk", `{"project":"demo","action":"stop"}`)
	if w.Code != http.StatusNotFound || len(fake.calls) != 0 || !strings.Contains(w.Body.String(), `"code":"project_has_no_containers"`) {
		t.Fatalf("status=%d calls=%v body=%s", w.Code, fake.calls, w.Body.String())
	}
}

func TestLifecycleBulkReportsPartialFailure(t *testing.T) {
	r := lifecycleRouter(t)
	fake := &fakeLifecycleDocker{containers: []types.Container{
		{ID: "good", State: "exited", Labels: map[string]string{"com.docker.compose.project": "demo"}},
		{ID: "bad", State: "exited", Labels: map[string]string{"com.docker.compose.project": "demo"}},
		{ID: "unrelated", State: "exited", Labels: map[string]string{"com.docker.compose.project": "other"}},
	}, actionErrors: map[string]error{"start:bad": errors.New("port busy")}}
	r.POST("/bulk", func(c *gin.Context) { handleBulkActionUsing(c, func() containerBulkClient { return fake }) })
	w := lifecycleRequest(r, "/bulk", `{"project":"demo","action":"start"}`)
	if w.Code != http.StatusMultiStatus || !strings.Contains(w.Body.String(), `"status":"partial"`) || !strings.Contains(w.Body.String(), `"failed":1`) || !strings.Contains(w.Body.String(), `"changed":1`) {
		t.Fatalf("resultat partiel masque: status=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Join(fake.calls, ",") != "start:good,start:bad" {
		t.Fatalf("cibles incorrectes: %v", fake.calls)
	}
}

func TestLifecycleBulkAllFailuresAreErrors(t *testing.T) {
	r := lifecycleRouter(t)
	fake := &fakeLifecycleDocker{containers: []types.Container{{ID: "bad", State: "running", Labels: map[string]string{"com.docker.compose.project": "demo"}}}, actionErrors: map[string]error{"stop:bad": context.DeadlineExceeded}}
	r.POST("/bulk", func(c *gin.Context) { handleBulkActionUsing(c, func() containerBulkClient { return fake }) })
	w := lifecycleRequest(r, "/bulk", `{"project":"demo","action":"stop"}`)
	if w.Code != http.StatusGatewayTimeout || !strings.Contains(w.Body.String(), `"status":"error"`) {
		t.Fatalf("faux succes: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestLifecycleBulkRestartAndAlreadyStopped(t *testing.T) {
	for _, tc := range []struct {
		action string
		calls  int
	}{{"restart", 1}, {"stop", 0}} {
		t.Run(tc.action, func(t *testing.T) {
			r := lifecycleRouter(t)
			fake := &fakeLifecycleDocker{containers: []types.Container{{ID: "web", State: "exited", Labels: map[string]string{"com.docker.compose.project": "demo"}}}}
			r.POST("/bulk", func(c *gin.Context) { handleBulkActionUsing(c, func() containerBulkClient { return fake }) })
			w := lifecycleRequest(r, "/bulk", `{"project":"demo","action":"`+tc.action+`"}`)
			if w.Code != http.StatusOK || len(fake.calls) != tc.calls {
				t.Fatalf("status=%d calls=%v body=%s", w.Code, fake.calls, w.Body.String())
			}
		})
	}
}
