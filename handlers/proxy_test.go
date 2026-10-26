package handlers

import (
	"net/http"
	"strings"
	"testing"
)

func TestRewriteProxyLocationKeepsLANTargetInsideViewProxy(t *testing.T) {
	targetHost := proxyTargetHost("localhost:18080")
	if targetHost == "" || targetHost == "127.0.0.1" || targetHost == "localhost" || targetHost == "::1" {
		t.Skip("no LAN proxy target on this host")
	}

	h := http.Header{}
	h.Set("Location", "https://"+targetHost+":8085/wp-admin/install.php")

	rewriteProxyLocation(h, "8085", "/view/iaif", "localhost:18080")

	if got, want := h.Get("Location"), "/view/iaif/wp-admin/install.php"; got != want {
		t.Fatalf("Location = %q, want %q", got, want)
	}
}

func TestRewriteProxyLocationKeepsRelativeRedirectInsideProxy(t *testing.T) {
	h := http.Header{}
	h.Set("Location", "/wp-admin/install.php")

	rewriteProxyLocation(h, "8085", "/view/iaif", "localhost:18080")

	if got, want := h.Get("Location"), "/view/iaif/wp-admin/install.php"; got != want {
		t.Fatalf("Location = %q, want %q", got, want)
	}
}

func TestRewriteProxyLocationIgnoresExternalRedirect(t *testing.T) {
	h := http.Header{}
	h.Set("Location", "https://example.com:8085/path")

	rewriteProxyLocation(h, "8085", "/view/iaif", "localhost:18080")

	if got := h.Get("Location"); !strings.HasPrefix(got, "https://example.com:8085/") {
		t.Fatalf("Location was unexpectedly rewritten: %q", got)
	}
}
