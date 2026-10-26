package config

import "testing"

func TestGeneratorDefaultsToRestrictedRoutesAndKeepsBypassLocal(t *testing.T) {
	for _, mode := range []string{"AUTH_BYPASS", "DEMO"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("AUTODOCKER_ROOT", t.TempDir())
			t.Setenv("BIND_HOST", "0.0.0.0")
			t.Setenv("AUTODOCKER_GENERATOR_ONLY", "")
			t.Setenv("AUTH_BYPASS", "0")
			t.Setenv("DEMO", "0")
			t.Setenv(mode, "1")
			Init()
			if BindHost != "127.0.0.1" || !GeneratorOnly {
				t.Fatal("unauthenticated POC can expose host control outside loopback")
			}
		})
	}
}

func TestLocalHTTPRequiresExplicitLoopbackBootstrap(t *testing.T) {
	t.Setenv("AUTODOCKER_ROOT", t.TempDir())
	t.Setenv("AUTH_BYPASS", "0")
	t.Setenv("DEMO", "0")
	t.Setenv("AUTODOCKER_LOCAL_BOOTSTRAP", "1")
	t.Setenv("AUTODOCKER_LOCAL_HTTP", "1")
	t.Setenv("BIND_HOST", "127.0.0.1")
	Init()
	if !NoSSL || !LocalBootstrapMode() {
		t.Fatal("local HTTP did not activate")
	}
	t.Setenv("BIND_HOST", "0.0.0.0")
	Init()
	if NoSSL || LocalBootstrapMode() {
		t.Fatal("HTTP activated on wildcard listener")
	}
	t.Setenv("BIND_HOST", "127.0.0.1")
	t.Setenv("AUTODOCKER_LOCAL_BOOTSTRAP", "0")
	Init()
	if NoSSL {
		t.Fatal("hosted install accepted local HTTP")
	}
}
