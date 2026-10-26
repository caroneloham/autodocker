package services

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

type infrastructureRouter struct {
	mu               sync.Mutex
	unavailableUntil map[string]time.Time
	clock            func() time.Time
	inFlight         map[string]bool
}

var infrastructureAutoRouter = newInfrastructureRouter()

func newInfrastructureRouter() *infrastructureRouter {
	return &infrastructureRouter{unavailableUntil: map[string]time.Time{}, inFlight: map[string]bool{}, clock: time.Now}
}

func infrastructureEnvDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

var infrastructureCredentialRotation = struct {
	sync.Mutex
	index map[string]uint64
}{index: map[string]uint64{}}

func infrastructureCredentials(provider string) []string {
	names := map[string][]string{
		"gemini":     {"GEMINI_API_KEYS", "GEMINI_API_KEY", "GEMINI_API_KEY_PROD"},
		"groq":       {"GROQ_API_KEYS", "GROQ_API_KEY"},
		"nvidia":     {"NVIDIA_API_KEYS", "NVIDIA_API_KEY"},
		"cloudflare": {"CLOUDFLARE_API_TOKENS", "CLOUDFLARE_API_TOKEN"},
	}
	if spec, known := cloudSpecFor(provider); known {
		names[provider] = cloudCredentialNames(spec)
	}
	var keys []string
	seen := map[string]bool{}
	for _, name := range names[provider] {
		for _, key := range strings.Split(os.Getenv(name), ",") {
			key = strings.TrimSpace(key)
			if key != "" && !seen[key] {
				seen[key] = true
				keys = append(keys, key)
			}
		}
	}
	// Keep support for credentials configured in the existing encrypted store.
	if len(keys) == 0 && provider == "gemini" {
		keyMu.Lock()
		keys = append(keys, apiKeys...)
		keyMu.Unlock()
	}
	if len(keys) == 0 && provider == "openrouter" {
		openrouterMu.Lock()
		keys = append(keys, openrouterKeys...)
		openrouterMu.Unlock()
	}
	return keys
}

// Rotation happens between user requests, never as repeated attempts against a
// quota-limited account. A 429 cools down the whole provider, including all keys.
func nextInfrastructureCredential(provider string) string {
	keys := infrastructureCredentials(provider)
	if len(keys) == 0 {
		return ""
	}
	infrastructureCredentialRotation.Lock()
	defer infrastructureCredentialRotation.Unlock()
	idx := infrastructureCredentialRotation.index[provider]
	infrastructureCredentialRotation.index[provider]++
	return keys[idx%uint64(len(keys))]
}

func infrastructureAutoProviders() ([]infrastructureProvider, error) {
	config, err := readCloudConfig()
	if err != nil {
		return nil, err
	}
	order := infrastructureEnvDefault("AUTODOCKER_AI_FALLBACKS", "groq,gemini,cloudflare,nvidia")
	if config.GenerationOrder != nil {
		order = strings.Join(config.GenerationOrder, ",")
	}
	seen := map[string]bool{}
	var providers []infrastructureProvider
	for _, name := range strings.Split(order, ",") {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		spec, known := cloudSpecFor(name)
		if !known || spec.Role != "generation" {
			return nil, fmt.Errorf("fournisseur de secours de génération non pris en charge")
		}
		if cloudEnabled(spec, config) {
			providers = append(providers, infrastructureProviderForName(name))
		}
	}
	if len(providers) == 0 {
		return nil, fmt.Errorf("aucune API de secours configurée")
	}
	return providers, nil
}

type infrastructureGenerateFunc func(context.Context, string, infrastructureProvider) (*MagicAIResult, error)

func (router *infrastructureRouter) generate(ctx context.Context, prompt string, providers []infrastructureProvider, generate infrastructureGenerateFunc) (*MagicAIResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 150*time.Second)
	defer cancel()
	var failed []string
	failedSeen := map[string]bool{}
	attempted := map[string]bool{}
	rejectedProposal := false
	for {
		busy := false
		var nextRetry time.Time
		for _, provider := range providers {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			router.mu.Lock()
			remaining := router.unavailableUntil[provider.name].Sub(router.clock())
			blocked := attempted[provider.name] || remaining > 0
			if until := router.unavailableUntil[provider.name]; !attempted[provider.name] && remaining > 0 && (nextRetry.IsZero() || until.Before(nextRetry)) {
				nextRetry = until
			}
			occupied := router.inFlight[provider.name]
			if !blocked && !occupied {
				router.inFlight[provider.name] = true
			}
			router.mu.Unlock()
			if blocked {
				if !failedSeen[provider.name] {
					failed = append(failed, provider.name)
					failedSeen[provider.name] = true
				}
				continue
			}
			if occupied {
				busy = true
				continue
			}
			attempted[provider.name] = true
			attemptCtx, stop := context.WithTimeout(ctx, 35*time.Second)
			result, err := generate(attemptCtx, prompt, provider)
			stop()
			router.mu.Lock()
			delete(router.inFlight, provider.name)
			router.mu.Unlock()
			if err == nil && result != nil {
				// Source reports the actual provider used, rather than the word auto.
				result.Source = provider.name
				if len(failed) > 0 {
					result.Warnings = append(result.Warnings, "Une API était indisponible. La proposition a été produite et vérifiée avec le fournisseur de secours "+provider.name+".")
				}
				return result, nil
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if err == nil {
				err = fmt.Errorf("réponse IA vide")
			}
			var proposal *infrastructureProposalError
			rejectedProposal = rejectedProposal || errors.As(err, &proposal)
			if cooldown := infrastructureCooldown(err); cooldown > 0 {
				router.mu.Lock()
				router.unavailableUntil[provider.name] = router.clock().Add(cooldown)
				router.mu.Unlock()
			}
			if !failedSeen[provider.name] {
				failed = append(failed, provider.name)
				failedSeen[provider.name] = true
			}
		}
		wait := 100 * time.Millisecond
		if !busy {
			// A new request may wait for a short shared cooldown instead of
			// failing immediately. Never retry a provider already tried by this
			// request, nor call it before its Retry-After has elapsed.
			deadline, _ := ctx.Deadline()
			if nextRetry.IsZero() || !nextRetry.Before(deadline) {
				break
			}
			wait = max(0, time.Until(nextRetry))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	if rejectedProposal {
		return nil, fmt.Errorf("aucune proposition valide n’a pu être préparée avec les API disponibles (%s). Certaines réponses n’ont pas passé les contrôles ; reformulez le besoin ou réessayez plus tard", strings.Join(failed, ", "))
	}
	return nil, fmt.Errorf("les API IA configurées sont temporairement indisponibles (%s). Leurs délais de reprise sont respectés ; réessayez plus tard ou utilisez le catalogue", strings.Join(failed, ", "))
}
