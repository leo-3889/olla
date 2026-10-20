package discovery

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/thushan/olla/internal/core/domain"
)

const (
	twoModelsNullCaps = `{"object":"list","data":[{"id":"model-a","object":"model","capabilities":null},{"id":"model-b","object":"model"}]}`
	twoModelsDeclared = `{"object":"list","data":[{"id":"model-a","object":"model","capabilities":{"image":true}},{"id":"model-b","object":"model","capabilities":{"audio":true}}]}`
	visionHealthTrue  = `{"status":"ok","vision":{"enabled":true,"max_pixels":3686400}}`
	visionHealthFalse = `{"status":"ok","vision":{"enabled":false}}`
	naiveHealth       = `{"status":"ok"}`
)

func capsTestServer(t *testing.T, modelsPayload, healthPayload string, healthHits *int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(modelsPayload))
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		if healthHits != nil {
			*healthHits++
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(healthPayload))
	})
	return httptest.NewServer(mux)
}

func capsTestEndpoint(url string) *domain.Endpoint {
	return &domain.Endpoint{
		Name:                 "caps-test",
		URLString:            url,
		Type:                 domain.ProfileOpenAICompatible,
		HealthCheckURLString: url + "/health",
	}
}

func newCapsClient(t *testing.T) *HTTPModelDiscoveryClient {
	t.Helper()
	client := NewHTTPModelDiscoveryClient(createTestProfileFactory(t), createTestLogger(), &http.Client{Timeout: 5 * time.Second})
	client.SetCapabilityProbe(true)
	return client
}

func capsOf(m *domain.ModelInfo) map[string]bool {
	if m.Details == nil {
		return nil
	}
	return m.Details.Capabilities
}

func findModel(models []*domain.ModelInfo, name string) *domain.ModelInfo {
	for _, m := range models {
		if m.Name == name {
			return m
		}
	}
	return nil
}

// W1: zero-cap endpoint + health vision=true -> EVERY model in the live list
// gets image=true (the live list is the scope: no phantom models exist).
func TestAugmentHealthProbeStampsLiveModels(t *testing.T) {
	srv := capsTestServer(t, twoModelsNullCaps, visionHealthTrue, nil)
	defer srv.Close()

	models, err := newCapsClient(t).DiscoverModels(context.Background(), capsTestEndpoint(srv.URL))
	if err != nil {
		t.Fatalf("DiscoverModels: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("want 2 models, got %d", len(models))
	}
	for _, name := range []string{"model-a", "model-b"} {
		m := findModel(models, name)
		if m == nil {
			t.Fatalf("model %s missing", name)
		}
		if !capsOf(m)["image"] {
			t.Errorf("model %s: want image=true from health probe, got %v", name, capsOf(m))
		}
	}
}

// W1 negative: naive /health (no vision block) and vision:false -> no stamps.
func TestAugmentHealthProbeNaiveNoOp(t *testing.T) {
	for _, health := range []string{naiveHealth, visionHealthFalse, `not json`} {
		srv := capsTestServer(t, twoModelsNullCaps, health, nil)
		models, err := newCapsClient(t).DiscoverModels(context.Background(), capsTestEndpoint(srv.URL))
		if err != nil {
			t.Fatalf("health %s: DiscoverModels: %v", health, err)
		}
		for _, m := range models {
			if len(capsOf(m)) != 0 {
				t.Errorf("health %s: model %s got caps %v, want none", health, m.Name, capsOf(m))
			}
		}
		srv.Close()
	}
}

// W1 self-retirement: an endpoint whose models declare ANY capability is
// never probed - zero requests to /health.
func TestAugmentDeclaringEngineNotProbed(t *testing.T) {
	hits := 0
	srv := capsTestServer(t, twoModelsDeclared, visionHealthTrue, &hits)
	defer srv.Close()

	if _, err := newCapsClient(t).DiscoverModels(context.Background(), capsTestEndpoint(srv.URL)); err != nil {
		t.Fatalf("DiscoverModels: %v", err)
	}
	if hits != 0 {
		t.Errorf("declaring engine: %d health probes, want 0", hits)
	}
}

// W2: a declaration applies ONLY to the exact model id on the live list;
// sibling models are untouched, engine declarations survive, and the stamp
// uses the ENGINE key (image) the unifier canonicalises.
func TestModelCapabilitiesScopedToId(t *testing.T) {
	srv := capsTestServer(t, twoModelsDeclared, naiveHealth, nil)
	defer srv.Close()

	client := newCapsClient(t)
	client.SetModelCapabilities(map[string][]string{
		"model-b": {"vision"},
		"ghost":   {"vision"}, // absent id: inert
	})
	models, err := client.DiscoverModels(context.Background(), capsTestEndpoint(srv.URL))
	if err != nil {
		t.Fatalf("DiscoverModels: %v", err)
	}

	a, b := findModel(models, "model-a"), findModel(models, "model-b")
	if a == nil || b == nil {
		t.Fatal("models missing")
	}
	if len(capsOf(a)) != 1 || !capsOf(a)["image"] {
		t.Errorf("model-a: want engine {image:true} untouched, got %v", capsOf(a))
	}
	if !capsOf(b)["audio"] || !capsOf(b)["image"] {
		t.Errorf("model-b: want {audio:true, image:true}, got %v", capsOf(b))
	}
	if len(capsOf(b)) != 2 {
		t.Errorf("model-b: unexpected extra caps %v", capsOf(b))
	}
}

// The engine-key vocabulary is the contract with the unifier's
// canonicalEngineCapabilities; pin it.
func TestCanonicalCapabilityFlagMap(t *testing.T) {
	want := map[string]string{
		"vision":              "image",
		"audio-transcription": "audio",
		"audio-speech":        "audio_generation",
	}
	if len(canonicalCapabilityFlags) != len(want) {
		t.Fatalf("vocabulary drift: %v", canonicalCapabilityFlags)
	}
	for name, flag := range want {
		if canonicalCapabilityFlags[name] != flag {
			t.Errorf("%s -> %s, want %s", name, canonicalCapabilityFlags[name], flag)
		}
	}
}
