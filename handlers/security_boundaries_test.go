package handlers

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"autodocker-go/config"
	"autodocker-go/db"
	"autodocker-go/middleware"
	"github.com/docker/docker/api/types/volume"
)

func TestPrivateConfigBackupCannotBeDownloadedThroughGenericRoute(t *testing.T) {
	r, client := accountTestRouter(t)
	oldBackups := config.BackupsDir
	config.BackupsDir = t.TempDir()
	t.Cleanup(func() { config.BackupsDir = oldBackups })
	if _, err := db.DB.Exec(`CREATE TABLE backup_history(id INTEGER PRIMARY KEY, project TEXT, filename TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(config.BackupsDir, "__autodocker__"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config.BackupsDir, "__autodocker__", "config.zip"), []byte("fictional test archive"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO backup_history(id,project,filename) VALUES(1,'__autodocker__','config.zip')`); err != nil {
		t.Fatal(err)
	}
	api := r.Group("/api", middleware.LoginRequired())
	RegisterBackupRoutes(api)

	for _, outsider := range []string{"alice", "legacy-admin"} {
		for _, path := range []string{"/api/backups/download/1", "/api/backups/config/download/1", "/api/backups/config/list", "/api/backups?project=__autodocker__"} {
			if response := client(outsider).request(http.MethodGet, path, ""); response.Code != http.StatusForbidden {
				t.Fatalf("%s accessed %s: %d", outsider, path, response.Code)
			}
		}
	}
	if response := client("owner").request(http.MethodGet, "/api/backups/download/1", ""); response.Code != http.StatusOK {
		t.Fatalf("owner download: %d", response.Code)
	}
}

func TestDirectReadProjectGrantRemainsReadable(t *testing.T) {
	_, _ = accountTestRouter(t)
	if _, err := db.DB.Exec(`CREATE TABLE labs_acl(project TEXT, username TEXT, role TEXT); CREATE TABLE labs(project TEXT, owner TEXT); CREATE TABLE groups_acl(group_name TEXT, project TEXT, role TEXT); CREATE TABLE user_groups(group_name TEXT, username TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO labs_acl(project,username,role) VALUES('mine','alice','read')`); err != nil {
		t.Fatal(err)
	}
	if got := getUserRoleOnProject("mine", "alice"); got != "read" {
		t.Fatalf("direct read grant became %q", got)
	}
}

func TestProjectMetadataListsRespectReadGrants(t *testing.T) {
	r, client := accountTestRouter(t)
	_, err := db.DB.Exec(`
		CREATE TABLE labs_acl(project TEXT, username TEXT, role TEXT);
		CREATE TABLE labs(project TEXT, owner TEXT);
		CREATE TABLE groups_acl(group_name TEXT, project TEXT, role TEXT);
		CREATE TABLE user_groups(group_name TEXT, username TEXT);
		CREATE TABLE secrets(id INTEGER PRIMARY KEY,project TEXT,key_name TEXT,created_by TEXT,created_at TEXT);
		CREATE TABLE scheduled_tasks(id INTEGER PRIMARY KEY,project TEXT,container_name TEXT,task_type TEXT,schedule_preset TEXT,command TEXT,enabled INTEGER,last_run TEXT,next_run TEXT,created_by TEXT,created_at TEXT);
		CREATE TABLE backup_schedules(id INTEGER PRIMARY KEY,project TEXT,volume_name TEXT,cron_expr TEXT,retention_count INTEGER,enabled INTEGER,last_run TEXT,created_by TEXT,created_at TEXT);
		INSERT INTO labs(project,owner) VALUES('mine','alice'),('other','bob');
		INSERT INTO secrets(project,key_name,created_by,created_at) VALUES('mine','MY_KEY','alice','today'),('other','OTHER_KEY','bob','today');
		INSERT INTO scheduled_tasks(project,container_name,task_type,schedule_preset,command,enabled,last_run,next_run,created_by,created_at) VALUES
		('mine','web','custom_command','every_hour','my-command',1,'','','alice','today'),
		('other','db','custom_command','every_hour','other-command',1,'','','bob','today');
		INSERT INTO backup_schedules(project,volume_name,cron_expr,retention_count,enabled,last_run,created_by,created_at) VALUES
		('mine','mine_data','0 2 * * *',5,1,'','alice','today'),
		('other','other_data','0 2 * * *',5,1,'','bob','today');
	`)
	if err != nil {
		t.Fatal(err)
	}
	api := r.Group("/api", middleware.LoginRequired())
	RegisterSecretsRoutes(api)
	RegisterScheduledTasksRoutes(api)
	RegisterBackupRoutes(api)
	alice := client("alice")
	if response := alice.request(http.MethodGet, "/api/secrets/other", ""); response.Code != http.StatusForbidden {
		t.Fatalf("other secret names exposed: %d", response.Code)
	}
	if response := alice.request(http.MethodGet, "/api/secrets/mine", ""); response.Code != http.StatusOK {
		t.Fatalf("own secret names denied: %d", response.Code)
	}
	response := alice.request(http.MethodGet, "/api/scheduled-tasks", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "my-command") || strings.Contains(response.Body.String(), "other-command") {
		t.Fatalf("scheduled command list crossed project boundary: %d %s", response.Code, response.Body.String())
	}
	response = alice.request(http.MethodGet, "/api/backups/schedules", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "mine_data") || strings.Contains(response.Body.String(), "other_data") {
		t.Fatalf("backup schedule list crossed project boundary: %d %s", response.Code, response.Body.String())
	}
}

func TestBackupComponentsRejectTraversalAndOtherVolumes(t *testing.T) {
	for _, value := range []string{"../secret", "foo/bar", "foo\\bar", "name;touch", "..", ""} {
		if validBackupComponent(value) {
			t.Fatalf("accepted component %q", value)
		}
	}
	if _, ok := backupFilePath("project", "../secret"); ok {
		t.Fatal("backup traversal accepted")
	}
	if backupVolumeAllowed(volume.Volume{Name: "other_data", Labels: map[string]string{"com.docker.compose.project": "other"}}, "project", false) {
		t.Fatal("cross-project volume accepted")
	}
	if !backupVolumeAllowed(volume.Volume{Name: "project_data", Labels: map[string]string{"com.docker.compose.project": "project"}}, "project", false) {
		t.Fatal("ordinary compose volume denied")
	}
}

func TestLocalProjectPathRejectsSiblingAndEscapingLink(t *testing.T) {
	base := t.TempDir()
	oldProjects := config.ProjectsDir
	config.ProjectsDir = filepath.Join(base, "projects")
	t.Cleanup(func() { config.ProjectsDir = oldProjects })
	project := filepath.Join(config.ProjectsDir, "mine")
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(project, "compose.yaml")
	if err := os.WriteFile(inside, []byte("services: {}"), 0600); err != nil {
		t.Fatal(err)
	}
	if name, ok := localProjectForPath(inside); !ok || name != "mine" {
		t.Fatal("ordinary project file denied")
	}
	sibling := filepath.Join(base, "projects-elsewhere")
	if err := os.MkdirAll(sibling, 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(sibling, "secret.txt")
	if err := os.WriteFile(outside, []byte("fictional secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, ok := localProjectForPath(outside); ok {
		t.Fatal("sibling prefix admitted")
	}
	link := filepath.Join(project, "linked-secret")
	if err := os.Symlink(outside, link); err == nil {
		if _, ok := localProjectForPath(link); ok {
			t.Fatal("escaping symlink admitted")
		}
	}
}

func TestTerminalRequiresSameOrigin(t *testing.T) {
	for _, tc := range []struct {
		origin  string
		allowed bool
	}{
		{"https://autodocker.example", true},
		{"https://evil.example", false},
		{"", false},
		{"https://autodocker.example.evil.example", false},
		{"https://autodocker.example/path", false},
	} {
		r, _ := http.NewRequest(http.MethodGet, "https://autodocker.example/ws/terminal", nil)
		r.Header.Set("Origin", tc.origin)
		if got := terminalSameOrigin(r); got != tc.allowed {
			t.Fatalf("origin %q allowed=%v", tc.origin, got)
		}
	}
}
