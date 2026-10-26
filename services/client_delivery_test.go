package services

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

func clientDeliveryTestPayload() BuildComposePayload {
	return BuildComposePayload{Client: &ClientDeliveryConfig{Name: "Atelier client", Description: "Plateforme privée", Accent: "#4f8cff", Support: "Référent infrastructure"}, Supervision: &SupervisionConfig{GPU: true, DiskPath: "/srv", DiskWarning: 85, MinFreeGiB: 15, TemperatureWarning: 85, BackupMaxHours: 30}, Services: []ServiceEntry{{Key: "nginx", Network: "app"}, {Key: "caddy", Network: "app", Image: "caddy:2-alpine"}}, Ingress: &IngressConfig{Proxy: "caddy", Domain: "localhost", Port: 18443, TLS: "internal"}, Addressing: AddressingConfig{Networks: []NetworkDef{{Name: "app", CIDR: "172.29.58.0/24", Gateway: "172.29.58.1"}}}}
}

func TestClientDeliveryIsOptionalAndPreservesExistingConnections(t *testing.T) {
	p := clientDeliveryTestPayload()
	if err := ValidateInfrastructureOptions(p); err != nil {
		t.Fatal(err)
	}
	c := BuildCompose(p, "client-example")
	files := ClientDeliveryProjectFiles(c)
	if len(files) != 5 || !strings.Contains(files["client.html"], "https://nginx.localhost:18443") || !strings.Contains(files["client.html"], "Atelier client") {
		t.Fatal("missing portal or actual proxy URL")
	}
	var cfg map[string]interface{}
	if err := json.Unmarshal([]byte(files["supervision.json"]), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["backup_status_file"] != "" {
		t.Fatal("unconfigured backup contract was omitted")
	}
	web := c["services"].(M)["nginx"].(M)
	if len(toStringList(web["ports"])) > 0 {
		t.Fatal("delivery changed proxy isolation")
	}
	p.Client = nil
	p.Supervision = nil
	if files = ClientDeliveryProjectFiles(BuildCompose(p, "no-delivery")); len(files) != 0 {
		t.Fatal("optional delivery leaked into projects")
	}
}

func TestClientDeliveryRejectsUnsafeInputBeforeFileGeneration(t *testing.T) {
	tests := []struct {
		name string
		edit func(*BuildComposePayload)
	}{
		{"html-logo", func(p *BuildComposePayload) { p.Client.Logo = "data:text/html;base64,PHNjcmlwdD4=" }},
		{"url-logo", func(p *BuildComposePayload) { p.Client.Logo = "https://example.test/logo.png" }},
		{"false-image", func(p *BuildComposePayload) {
			p.Client.Logo = "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("not an image"))
		}},
		{"css-injection", func(p *BuildComposePayload) { p.Client.Accent = "red;display:none" }},
		{"control", func(p *BuildComposePayload) { p.Client.Name = "name\nnew" }},
		{"threshold", func(p *BuildComposePayload) { p.Supervision.DiskWarning = 101 }},
		{"path-control", func(p *BuildComposePayload) { p.Supervision.BackupStatusFile = "/tmp/a\n" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := clientDeliveryTestPayload()
			tt.edit(&p)
			if ValidateInfrastructureOptions(p) == nil {
				t.Fatal("unsafe option accepted")
			}
			if len(ClientDeliveryProjectFiles(BuildCompose(p, "invalid"))) != 0 {
				t.Fatal("invalid input generated files")
			}
		})
	}
}

func TestClientPortalEscapesClientTextAndAcceptsOnlyValidatedRasterLogo(t *testing.T) {
	p := clientDeliveryTestPayload()
	p.Client.Name = `Client <script>alert("x")</script>`
	var logo bytes.Buffer
	if err := png.Encode(&logo, image.NewRGBA(image.Rect(0, 0, 24, 24))); err != nil {
		t.Fatal(err)
	}
	p.Client.Logo = "data:image/png;base64," + base64.StdEncoding.EncodeToString(logo.Bytes())
	if err := ValidateInfrastructureOptions(p); err != nil {
		t.Fatal(err)
	}
	page := ClientDeliveryProjectFiles(BuildCompose(p, "logo"))["client.html"]
	if strings.Contains(page, `<script>alert("x")`) || strings.Contains(page, "#ZgotmplZ") || !strings.Contains(page, "data:image/png;base64,") {
		t.Fatal("unsafe or broken portal rendering")
	}
	p.Supervision = nil
	page = ClientDeliveryProjectFiles(BuildCompose(p, "branding-only"))["client.html"]
	if strings.Contains(page, "/status.json") {
		t.Fatal("branding-only portal polls missing collector")
	}
}

func TestClientDeliveryComposeIsAcceptedByDockerWithoutHostAccess(t *testing.T) {
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("Docker CLI unavailable")
	}
	p := clientDeliveryTestPayload()
	p.Client.Name = "Atelier validation"
	data, err := yaml.Marshal(BuildCompose(p, "delivery-validation"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "compose.yaml")
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(docker, "compose", "-f", path, "config", "--quiet")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compose rejected: %v %s", err, output)
	}
}

func TestGeneratedSupervisionRunsOnRealHostWithoutClaimingBackupSuccess(t *testing.T) {
	python, err := exec.LookPath("python")
	if err != nil {
		t.Skip("Python unavailable")
	}
	p := clientDeliveryTestPayload()
	p.Supervision.GPU = false
	p.Supervision.DiskPath = t.TempDir()
	p.Supervision.MinFreeGiB = 1
	files := ClientDeliveryProjectFiles(BuildCompose(p, "host-report"))
	root := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(python, "-X", "utf8", filepath.Join(root, "supervision.py"), "--once")
	if err := cmd.Run(); err == nil {
		t.Fatal("unconfigured backup produced a healthy exit status")
	}
	data, err := os.ReadFile(filepath.Join(root, "supervision-status.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		State  string `json:"state"`
		Checks []struct {
			State string `json:"state"`
		} `json:"checks"`
	}
	if err = json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if report.State == "ok" || len(report.Checks) != 3 || report.Checks[2].State != "not_configured" {
		t.Fatal("report misrepresents an absent backup")
	}
}
