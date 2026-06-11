package handlers

import (
	"sort"
	"testing"
	"time"

	"github.com/thushan/olla/internal/core/domain"
)

// TestBuildModelSummaries_EndpointNamesConsistent pins the v0.0.28 contract:
// ModelSummary.Endpoints must contain endpoint NAMES (not URLs) for all
// occurrences — first and subsequent — when a model appears on multiple
// endpoints.
//
// Prior to the Phase 1 pool-removal refactor the first occurrence received the
// endpoint URL while duplicates received the name. The new code is consistent
// (always names). This test exists to lock that contract so it cannot silently
// drift back.
func TestBuildModelSummaries_EndpointNamesConsistent(t *testing.T) {
	t.Parallel()

	app := &Application{}

	now := time.Now()

	modelMap := map[string]*domain.EndpointModels{
		"http://localhost:11434": {
			Models: []*domain.ModelInfo{
				{Name: "llama3", LastSeen: now},
				{Name: "mistral", LastSeen: now},
			},
		},
		"http://localhost:8080": {
			Models: []*domain.ModelInfo{
				// llama3 appears on both endpoints — the duplicate path was the
				// original source of the inconsistency.
				{Name: "llama3", LastSeen: now.Add(-time.Minute)},
			},
		},
	}

	// endpointNames maps URL → human-readable name, mirroring what modelsStatusHandler builds.
	endpointNames := map[string]string{
		"http://localhost:11434": "ollama-local",
		"http://localhost:8080":  "lmstudio-local",
	}

	summaries := app.buildModelSummaries(modelMap, endpointNames)

	// Build a lookup by model name for assertion convenience.
	byName := make(map[string]ModelSummary, len(summaries))
	for _, s := range summaries {
		byName[s.Name] = s
	}

	// mistral lives on exactly one endpoint — straightforward name check.
	mistral, ok := byName["mistral"]
	if !ok {
		t.Fatal("expected 'mistral' in summaries")
	}
	if len(mistral.Endpoints) != 1 {
		t.Fatalf("mistral: expected 1 endpoint, got %d: %v", len(mistral.Endpoints), mistral.Endpoints)
	}
	if mistral.Endpoints[0] != "ollama-local" {
		t.Errorf("mistral: Endpoints[0] should be name 'ollama-local', got %q", mistral.Endpoints[0])
	}

	// llama3 lives on two endpoints — this is where old code put a URL for the
	// first occurrence. Both entries must be names.
	llama, ok := byName["llama3"]
	if !ok {
		t.Fatal("expected 'llama3' in summaries")
	}
	if len(llama.Endpoints) != 2 {
		t.Fatalf("llama3: expected 2 endpoints, got %d: %v", len(llama.Endpoints), llama.Endpoints)
	}

	sort.Strings(llama.Endpoints)
	wantEndpoints := []string{"lmstudio-local", "ollama-local"}
	sort.Strings(wantEndpoints)

	for i, got := range llama.Endpoints {
		if got != wantEndpoints[i] {
			t.Errorf("llama3: Endpoints[%d] = %q, want %q (must be a name, not a URL)", i, got, wantEndpoints[i])
		}
	}

	// Paranoia: verify none of the endpoint strings look like URLs.
	for _, s := range summaries {
		for _, ep := range s.Endpoints {
			if len(ep) > 4 && ep[:4] == "http" {
				t.Errorf("model %q: endpoint %q looks like a URL, expected an endpoint name", s.Name, ep)
			}
		}
	}
}

// TestBuildModelSummaries_FallbackToURL confirms that when a URL has no name
// mapping (e.g. a newly-discovered endpoint not yet in the repository snapshot),
// buildModelSummaries falls back to the URL so the model is not silently
// dropped. The URL-as-fallback is an explicit code path (endpointName = endpointURL).
func TestBuildModelSummaries_FallbackToURL(t *testing.T) {
	t.Parallel()

	app := &Application{}

	modelMap := map[string]*domain.EndpointModels{
		"http://192.168.1.50:11434": {
			Models: []*domain.ModelInfo{
				{Name: "phi3", LastSeen: time.Now()},
			},
		},
	}

	// No entry for this URL — triggers the fallback to URL path.
	endpointNames := map[string]string{}

	summaries := app.buildModelSummaries(modelMap, endpointNames)

	if len(summaries) != 1 {
		t.Fatalf("expected 1 summary, got %d", len(summaries))
	}
	if summaries[0].Name != "phi3" {
		t.Fatalf("unexpected model name %q", summaries[0].Name)
	}
	if len(summaries[0].Endpoints) != 1 {
		t.Fatalf("expected 1 endpoint entry, got %d", len(summaries[0].Endpoints))
	}
	// The fallback value is the URL itself — this is acceptable and documented behaviour.
	if summaries[0].Endpoints[0] != "http://192.168.1.50:11434" {
		t.Errorf("expected URL as fallback endpoint, got %q", summaries[0].Endpoints[0])
	}
}
