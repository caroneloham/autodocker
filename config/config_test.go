package config

import "testing"

func TestLocalSessionsAreSeparatedByPortAndHostedNameIsStable(t *testing.T) {
	oldBind, oldPort, oldBypass, oldDemo := BindHost, Port, AuthBypass, DemoMode
	t.Cleanup(func() { BindHost, Port, AuthBypass, DemoMode = oldBind, oldPort, oldBypass, oldDemo })
	t.Setenv("AUTODOCKER_LOCAL_BOOTSTRAP", "1")
	BindHost, AuthBypass, DemoMode = "127.0.0.1", false, false
	Port = "18743"
	first := SessionCookieName()
	Port = "18745"
	if first == SessionCookieName() || first == "session" {
		t.Fatal("independent localhost ports overwrite the same cookie")
	}
	t.Setenv("AUTODOCKER_LOCAL_BOOTSTRAP", "0")
	if SessionCookieName() != "session" {
		t.Fatal("hosted sessions would be invalidated by the local cookie change")
	}
}

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
