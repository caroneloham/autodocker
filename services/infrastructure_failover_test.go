package services

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestInfrastructureHTTPFailuresHideSecretsAndHonorRetryAfter(t *testing.T) {
	for _, status := range []int{401, 403, 429, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer synthetic-secret" {
					t.Error("missing authorization")
				}
				w.Header().Set("Retry-After", "120")
				w.WriteHeader(status)
				w.Write([]byte(`{"error":"synthetic-secret and private prompt"}`))
			}))
			defer server.Close()
			_, err := requestInfrastructureModel(context.Background(), server.URL, "test", "synthetic-secret", "openai", "system", "private prompt")
			var remote *infrastructureProviderError
			if !errors.As(err, &remote) || remote.status != status || infrastructureCooldown(err) != 120*time.Second {
				t.Fatalf("failure not classified safely: %v", err)
			}
			if strings.Contains(err.Error(), "synthetic-secret") || strings.Contains(err.Error(), "private prompt") || strings.Contains(err.Error(), server.URL) {
				t.Fatal("secret data leaked")
			}
		})
	}
	now := time.Now().UTC().Truncate(time.Second)
	if got := infrastructureRetryAfter(now.Add(90*time.Second).Format(http.TimeFormat), now); got != 90*time.Second {
		t.Fatal(got)
	}
}

func TestInfrastructureGeminiCredentialUsesHeaderAndNoRedirect(t *testing.T) {
	called := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != "synthetic-secret" || r.URL.RawQuery != "" {
			t.Error("Gemini key must be a header")
		}
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	if _, err := requestInfrastructureModel(context.Background(), server.URL, "test", "synthetic-secret", "gemini", "system", "message"); err == nil || called {
		t.Fatal("credential-bearing redirect was followed")
	}
}

func TestInfrastructureAutoFallbackKeepsValidationAndProviderCooldown(t *testing.T) {
	router := newInfrastructureRouter()
	now := time.Now()
	router.clock = func() time.Time { return now }
	calls := map[string]int{}
	providers := []infrastructureProvider{{name: "groq"}, {name: "nvidia"}}
	run := func(ctx context.Context, prompt string, provider infrastructureProvider) (*MagicAIResult, error) {
		calls[provider.name]++
		if provider.name == "groq" {
			return nil, &infrastructureProviderError{status: 429, retryAfter: 2 * time.Minute, message: "quota"}
		}
		provider.complete = func(context.Context, string, string) (string, error) { return infrastructureAnswer("wordpress"), nil }
		return generateInfrastructureWithProvider(ctx, prompt, provider)
	}
	for i := 0; i < 2; i++ {
		result, err := router.generate(context.Background(), "WordPress et MariaDB", providers, run)
		if err != nil || result.Source != "nvidia" || result.Confidence != "validated" || len(result.Warnings) == 0 {
			t.Fatalf("fallback lost validation: %v", err)
		}
		encoded, _ := json.Marshal(result.Payload)
		if !strings.Contains(string(encoded), "mariadb") {
			t.Fatal("dependency lost after failover")
		}
	}
	if calls["groq"] != 1 || calls["nvidia"] != 2 {
		t.Fatalf("cooldown bypassed: %v", calls)
	}
	now = now.Add(121 * time.Second)
	router.generate(context.Background(), "WordPress", providers, run)
	if calls["groq"] != 2 {
		t.Fatal("provider never recovered")
	}
}

func TestInfrastructureAutoCancellationDoesNotCallAnotherProvider(t *testing.T) {
	router := newInfrastructureRouter()
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	_, err := router.generate(ctx, "WordPress", []infrastructureProvider{{name: "groq"}, {name: "nvidia"}}, func(context.Context, string, infrastructureProvider) (*MagicAIResult, error) {
		calls++
		cancel()
		return nil, context.Canceled
	})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("cancel ignored: %d, %v", calls, err)
	}
}

func TestInvalidProposalDoesNotSuspendProviderForOtherRequests(t *testing.T) {
	router := newInfrastructureRouter()
	providers := []infrastructureProvider{{name: "groq"}, {name: "nvidia"}}
	run := func(ctx context.Context, prompt string, p infrastructureProvider) (*MagicAIResult, error) {
		p.complete = func(_ context.Context, _, message string) (string, error) {
			if p.name == "groq" && strings.Contains(message, "configuration invalide") {
				return "not JSON", nil
			}
			return infrastructureAnswer("nginx"), nil
		}
		return generateInfrastructureWithProvider(ctx, prompt, p)
	}
	first, err := router.generate(context.Background(), "Nginx avec la configuration invalide", providers, run)
	if err != nil || first.Source != "nvidia" {
		t.Fatalf("invalid response should fail over: %v", err)
	}
	second, err := router.generate(context.Background(), "Nginx seul", providers, run)
	if err != nil || second.Source != "groq" {
		t.Fatalf("one rejected proposal suspended an API for other users: %v", err)
	}
}

func TestInfrastructureWaitsForSharedCooldownWithoutEarlyAPICall(t *testing.T) {
	router := newInfrastructureRouter()
	until := time.Now().Add(25 * time.Millisecond)
	router.unavailableUntil["groq"] = until
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	calls := 0
	_, err := router.generate(ctx, "Nginx", []infrastructureProvider{{name: "groq"}}, func(context.Context, string, infrastructureProvider) (*MagicAIResult, error) {
		calls++
		if time.Now().Before(until) {
			t.Error("Retry-After was bypassed")
		}
		return &MagicAIResult{Confidence: "validated"}, nil
	})
	if err != nil || calls != 1 {
		t.Fatalf("short shared cooldown rejected the waiting request: %v", err)
	}
}

func TestInfrastructureCooldownWaitCanBeCancelled(t *testing.T) {
	router := newInfrastructureRouter()
	router.unavailableUntil["groq"] = time.Now().Add(time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	timer := time.AfterFunc(10*time.Millisecond, cancel)
	defer timer.Stop()
	calls := 0
	_, err := router.generate(ctx, "Nginx", []infrastructureProvider{{name: "groq"}}, func(context.Context, string, infrastructureProvider) (*MagicAIResult, error) {
		calls++
		return &MagicAIResult{}, nil
	})
	if !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatalf("waiting cancellation called an API: %v, calls=%d", err, calls)
	}
}

func TestInfrastructureCredentialsAreDeduplicatedAndRotateBetweenRequests(t *testing.T) {
	t.Setenv("NVIDIA_API_KEYS", "synthetic-one,synthetic-two")
	t.Setenv("NVIDIA_API_KEY", "synthetic-one")
	keys := infrastructureCredentials("nvidia")
	if len(keys) != 2 {
		t.Fatal("duplicate key was retained")
	}
	a, b, c := nextInfrastructureCredential("nvidia"), nextInfrastructureCredential("nvidia"), nextInfrastructureCredential("nvidia")
	if a == b || a != c {
		t.Fatal("rotation failed")
	}
}

func TestInfrastructureAutoAllowlistAndCancelledGeneration(t *testing.T) {
	t.Setenv("AUTODOCKER_AI_FALLBACKS", "https://arbitrary.example")
	if _, err := infrastructureAutoProviders(); err == nil {
		t.Fatal("unrecognized provider accepted")
	}
	t.Setenv("AUTODOCKER_AI_PROVIDER", "auto")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	defer cancel()
	if _, err := GenerateInfrastructureConfig(ctx, "WordPress, 4 Go RAM et 2 CPU, accès local"); err == nil {
		t.Fatalf("cancelled or invalid generation did not fail")
	}
}

func TestInfrastructureCloudflareEnvelopeAndNvidiaThinkingOptions(t *testing.T) {
	for _, format := range []string{"cloudflare", "nvidia"} {
		t.Run(format, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]interface{}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if format == "cloudflare" {
					if body["options"].(map[string]interface{})["rejectIfBusy"] != true {
						t.Error("capacity queue must be bounded")
					}
					json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "result": map[string]string{"response": "synthetic-answer"}})
				} else {
					if body["chat_template_kwargs"].(map[string]interface{})["enable_thinking"] != false {
						t.Error("long reasoning must be disabled")
					}
					json.NewEncoder(w).Encode(map[string]interface{}{"choices": []interface{}{map[string]interface{}{"message": map[string]string{"content": "synthetic-answer"}}}})
				}
			}))
			defer server.Close()
			text, err := requestInfrastructureModel(context.Background(), server.URL, "nvidia/nemotron-3-super-120b-a12b", "synthetic-secret", format, "system", "message")
			if err != nil || text != "synthetic-answer" {
				t.Fatalf("protocol not handled: %v", err)
			}
		})
	}
}

func TestInfrastructureCloudflareJSONContentReachesSharedValidation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var answer map[string]interface{}
		json.Unmarshal([]byte(infrastructureAnswer("wordpress")), &answer)
		json.NewEncoder(w).Encode(map[string]interface{}{"choices": []interface{}{map[string]interface{}{"message": map[string]interface{}{"content": answer}}}})
	}))
	defer server.Close()
	p := infrastructureProvider{name: "cloudflare", complete: func(ctx context.Context, system, message string) (string, error) {
		return requestInfrastructureModel(ctx, server.URL, "test", "synthetic-secret", "cloudflare", system, message)
	}}
	result, err := generateInfrastructureWithProvider(context.Background(), "WordPress", p)
	if err != nil || result.Confidence != "validated" || !infrastructureKeys(result)["mariadb"] {
		t.Fatalf("structured JSON content was lost: %v", err)
	}
}
