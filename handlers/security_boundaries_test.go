package handlers

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"autodocker-go/config"
	"autodocker-go/db"
	"autodocker-go/middleware"
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

func TestBackupComponentsRejectTraversalAndOtherVolumes(t *testing.T) {
	for _, value := range []string{"../secret", "foo/bar", "foo\\bar", "name;touch", "..", ""} {
		if validBackupComponent(value) {
			t.Fatalf("accepted component %q", value)
		}
	}
	if _, ok := backupFilePath("project", "../secret"); ok {
		t.Fatal("backup traversal accepted")
	}
	if volumeBelongsToBackupProject("other_data", "project") {
		t.Fatal("cross-project volume accepted")
	}
	if !volumeBelongsToBackupProject("project_data", "project") {
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
