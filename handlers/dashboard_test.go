package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"autodocker-go/db"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/volume"
	"github.com/gin-gonic/gin"
)

type fakeContainerLister struct {
	containers []types.Container
}

func (f fakeContainerLister) ContainerList(context.Context, container.ListOptions) ([]types.Container, error) {
	return f.containers, nil
}

type fakeVolumeRemover struct {
	volumes []*volume.Volume
	removed []string
}

func (f *fakeVolumeRemover) VolumeList(context.Context, volume.ListOptions) (volume.ListResponse, error) {
	return volume.ListResponse{Volumes: f.volumes}, nil
}

func (f *fakeVolumeRemover) VolumeRemove(_ context.Context, name string, _ bool) error {
	f.removed = append(f.removed, name)
	return nil
}

func TestShortPortParsingAndReplacement(t *testing.T) {
	port, ok := parseShortPublishedPort("127.0.0.1:3000:80/tcp")
	if !ok || port != 3000 {
		t.Fatalf("port attendu 3000, recu %d ok=%v", port, ok)
	}
	if got := replaceShortPublishedPort("127.0.0.1:3000:80/tcp", 3001); got != "127.0.0.1:3001:80/tcp" {
		t.Fatalf("remplacement inattendu: %s", got)
	}
	if _, ok := parseShortPublishedPort("${WEB_PORT}:80"); ok {
		t.Fatal("les ports variables ne doivent pas etre remappes automatiquement")
	}
	if _, ok := parseShortPublishedPort("8080-8082:80-82"); ok {
		t.Fatal("les plages de ports ne doivent pas etre remappees automatiquement")
	}
}

func TestDockerUnavailablePayloadIsActionable(t *testing.T) {
	payload := dockerUnavailablePayload()
	if payload["code"] != "docker_unavailable" {
		t.Fatalf("code docker_unavailable attendu, recu %#v", payload["code"])
	}
	if payload["message"] == "" || payload["detail"] == "" {
		t.Fatalf("message et detail requis: %#v", payload)
	}
	actions, ok := payload["actions"].([]gin.H)
	if !ok || len(actions) < 2 {
		t.Fatalf("actions utilisateur attendues, recu %#v", payload["actions"])
	}
}

func TestVolumeBelongsToProject(t *testing.T) {
	tests := []struct {
		name    string
		volume  *volume.Volume
		project string
		want    bool
	}{
		{
			name:    "compose label",
			volume:  &volume.Volume{Name: "random", Labels: map[string]string{"com.docker.compose.project": "demo"}},
			project: "demo",
			want:    true,
		},
		{
			name:    "compose prefix",
			volume:  &volume.Volume{Name: "demo_postgres_data"},
			project: "demo",
			want:    true,
		},
		{
			name:    "similar name without separator",
			volume:  &volume.Volume{Name: "demolition_data"},
			project: "demo",
			want:    false,
		},
		{
			name:    "other project label wins over prefix",
			volume:  &volume.Volume{Name: "demo_data", Labels: map[string]string{"com.docker.compose.project": "other"}},
			project: "demo",
			want:    false,
		},
		{
			name:    "matching project label",
			volume:  &volume.Volume{Name: "other_data", Labels: map[string]string{"com.docker.compose.project": "demo"}},
			project: "demo",
			want:    true,
		},
		{
			name:    "nil volume",
			volume:  nil,
			project: "demo",
			want:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := volumeBelongsToProject(tt.volume, tt.project); got != tt.want {
				t.Fatalf("volumeBelongsToProject() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRemoveProjectDockerVolumesOnlyTargetsProject(t *testing.T) {
	client := &fakeVolumeRemover{volumes: []*volume.Volume{
		{Name: "demo_db_data"},
		{Name: "demo_cache", Labels: map[string]string{"com.docker.compose.project": "other"}},
		{Name: "other_data", Labels: map[string]string{"com.docker.compose.project": "demo"}},
		{Name: "demolition_data"},
	}}

	removed, err := removeProjectDockerVolumes(context.Background(), client, "demo")
	if err != nil {
		t.Fatalf("removeProjectDockerVolumes: %v", err)
	}
	if removed != 2 {
		t.Fatalf("2 volumes supprimes attendus, recu %d", removed)
	}
	got := strings.Join(client.removed, ",")
	if got != "demo_db_data,other_data" {
		t.Fatalf("volumes supprimes inattendus: %s", got)
	}
}

func TestBuildProjectLogDigestPrioritizesProblems(t *testing.T) {
	digest := buildProjectLogDigest([]projectLogEntry{
		{Timestamp: "2026-06-27T10:03:00Z", ContainerName: "web", Severity: "info", Message: "server ready"},
		{Timestamp: "2026-06-27T10:02:00Z", ContainerName: "db", Severity: "warning", Message: "  slow query detected  "},
		{Timestamp: "2026-06-27T10:01:00Z", ContainerName: "worker", Severity: "fatal", Message: "connection failed"},
	})

	if digest.Summary.Tone != "error" {
		t.Fatalf("tone erreur attendu, recu %q", digest.Summary.Tone)
	}
	if digest.Counts["error"] != 1 || digest.Counts["warning"] != 1 || digest.Counts["info"] != 1 {
		t.Fatalf("counts inattendus: %#v", digest.Counts)
	}
	if len(digest.Highlights) != 3 {
		t.Fatalf("3 highlights attendus, recu %d", len(digest.Highlights))
	}
	if digest.Highlights[0].Severity != "error" || digest.Highlights[1].Severity != "warning" {
		t.Fatalf("les erreurs doivent passer avant les avertissements et infos: %#v", digest.Highlights)
	}
	if digest.Highlights[1].Message != "slow query detected" {
		t.Fatalf("message non normalise: %q", digest.Highlights[1].Message)
	}
	if digest.Advice.Tone != "warn" && digest.Advice.Tone != "error" {
		t.Fatalf("conseil utilisateur attendu sur probleme, recu %#v", digest.Advice)
	}
}

func TestBuildProjectLogDigestEmpty(t *testing.T) {
	digest := buildProjectLogDigest(nil)
	if digest.Summary.Tone != "muted" {
		t.Fatalf("tone muted attendu, recu %q", digest.Summary.Tone)
	}
	if digest.Total != 0 || len(digest.Highlights) != 0 {
		t.Fatalf("digest vide inattendu: %#v", digest)
	}
	if digest.RecommendedAction == "" {
		t.Fatal("une action recommandee est attendue meme sans log")
	}
	if digest.Advice.Meaning == "" || digest.Advice.NextStep == "" {
		t.Fatalf("conseil humain attendu meme sans log: %#v", digest.Advice)
	}
}

func TestBuildProjectLogDigestExplainsBusyPort(t *testing.T) {
	digest := buildProjectLogDigest([]projectLogEntry{
		{Timestamp: "2026-06-27T10:01:00Z", ContainerName: "web", Severity: "error", Message: "listen tcp 0.0.0.0:8080: bind: address already in use"},
	})
	if digest.Advice.Tone != "warn" {
		t.Fatalf("tone warn attendu pour port occupe, recu %#v", digest.Advice)
	}
	if !strings.Contains(strings.ToLower(digest.Advice.Meaning), "port") {
		t.Fatalf("explication port attendue, recu %#v", digest.Advice)
	}
	if digest.Advice.NextStep == "" {
		t.Fatalf("prochaine action attendue, recu %#v", digest.Advice)
	}
}

func TestDeleteProjectMetadataRemovesProjectRowsOnly(t *testing.T) {
	oldDB := db.DB
	testDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sqlite memoire: %v", err)
	}
	defer testDB.Close()
	db.DB = testDB
	defer func() { db.DB = oldDB }()

	tables := []string{
		"labs_acl",
		"groups_acl",
		"backup_schedules",
		"secrets",
		"scheduled_tasks",
		"container_logs",
		"uptime_events",
		"scaling_events",
		"metrics_history",
		"labs",
	}
	for _, table := range tables {
		if _, err := testDB.Exec("CREATE TABLE " + table + " (project TEXT)"); err != nil {
			t.Fatalf("create %s: %v", table, err)
		}
		if _, err := testDB.Exec("INSERT INTO " + table + " (project) VALUES ('demo'), ('keep')"); err != nil {
			t.Fatalf("insert %s: %v", table, err)
		}
	}

	removed, err := deleteProjectMetadata("demo")
	if err != nil {
		t.Fatalf("deleteProjectMetadata: %v", err)
	}
	if removed != int64(len(tables)) {
		t.Fatalf("%d lignes supprimees attendues, recu %d", len(tables), removed)
	}

	for _, table := range tables {
		var demoCount, keepCount int
		if err := testDB.QueryRow("SELECT COUNT(*) FROM " + table + " WHERE project='demo'").Scan(&demoCount); err != nil {
			t.Fatalf("count demo %s: %v", table, err)
		}
		if err := testDB.QueryRow("SELECT COUNT(*) FROM " + table + " WHERE project='keep'").Scan(&keepCount); err != nil {
			t.Fatalf("count keep %s: %v", table, err)
		}
		if demoCount != 0 || keepCount != 1 {
			t.Fatalf("%s: demo=%d keep=%d", table, demoCount, keepCount)
		}
	}
}

func TestRemapBusyComposePortsWritesVersionedCompose(t *testing.T) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("ecoute port test: %v", err)
	}
	defer ln.Close()
	busyPort := ln.Addr().(*net.TCPAddr).Port

	dir := t.TempDir()
	composePath := filepath.Join(dir, "compose.yaml")
	compose := fmt.Sprintf("services:\n  web:\n    image: nginx:alpine\n    ports:\n      - \"%d:80\"\n", busyPort)
	if err := os.WriteFile(composePath, []byte(compose), 0644); err != nil {
		t.Fatalf("ecriture compose: %v", err)
	}

	remaps, err := remapBusyComposePorts(context.Background(), fakeContainerLister{}, "demo", dir, composePath)
	if err != nil {
		t.Fatalf("remapBusyComposePorts: %v", err)
	}
	if len(remaps) != 1 {
		t.Fatalf("1 remap attendu, recu %#v", remaps)
	}
	if remaps[0].OldPort != busyPort || remaps[0].NewPort == busyPort {
		t.Fatalf("remap incoherent: %#v", remaps[0])
	}

	updated, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatalf("lecture compose mis a jour: %v", err)
	}
	if strings.Contains(string(updated), fmt.Sprintf("%d:80", busyPort)) {
		t.Fatalf("ancien port encore present dans compose:\n%s", updated)
	}
	if !strings.Contains(string(updated), fmt.Sprintf("%d:80", remaps[0].NewPort)) {
		t.Fatalf("nouveau port absent du compose:\n%s", updated)
	}

	versions, err := os.ReadDir(filepath.Join(dir, "versions"))
	if err != nil {
		t.Fatalf("versions manquantes: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("1 snapshot attendu, recu %d", len(versions))
	}
}
