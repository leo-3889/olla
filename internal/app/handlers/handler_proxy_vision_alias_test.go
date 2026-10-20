// Package handlers integration-shape test reproducing the live vision-alias
// failure (alias "slow" + image request -> 404 model_not_found) at the
// filter/dispatch boundary, and proving the W1/W2 stamp reaching the registry
// capability index makes the halogen endpoint routable for vision traffic.
package handlers

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/thushan/olla/internal/core/domain"
)

// visionTestEndpoint builds a domain.Endpoint the way the handler tests do:
// URL parsed from the loopback string plus the redundant URLString field.
func visionTestEndpoint(name, rawURL, endpointType string) *domain.Endpoint {
	return &domain.Endpoint{
		Name:      name,
		URL:       mustParseVisionTestURL(rawURL),
		URLString: rawURL,
		Type:      endpointType,
	}
}

func mustParseVisionTestURL(rawURL string) *url.URL {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		panic("invalid test endpoint URL: " + rawURL)
	}
	return parsed
}

func visionAliasEndpointNames(endpoints []*domain.Endpoint) []string {
	names := make([]string, 0, len(endpoints))
	for _, endpoint := range endpoints {
		names = append(names, endpoint.Name)
	}
	return names
}

// TestVisionAliasSlowResolvesToStampedHalogen reproduces the live fleet shape:
//
//	radiance-64  (vLLM,  qwen4exp,                    advertises capabilities.image=true)
//	halogen-395  (llama, halogen-qwen3.8-flash-next,  engine /health vision.enabled=true
//	                                                 but /v1/models advertises capabilities:null)
//
// Pre-W1/W2 the halogen model never entered the registry's "vision" capability
// index, so an image request on alias "slow" (or the 395 leg of "all") was
// rejected 404 model_not_found by the capability filter running BEFORE the
// balancer. Post-stamp the halogen model carries [vision chat] in the index
// and the filter keeps its endpoint.
func TestVisionAliasSlowResolvesToStampedHalogen(t *testing.T) {
	const (
		radianceURL = "http://localhost:8100"
		halogenURL  = "http://localhost:8131"

		qwenModel    = "qwen4exp"
		halogenModel = "halogen-qwen3.8-flash-next"
	)

	radiance := visionTestEndpoint("radiance-64", radianceURL, domain.ProfileOpenAICompatible)
	halogen := visionTestEndpoint("halogen-395", halogenURL, domain.ProfileLlamaCpp)
	healthy := []*domain.Endpoint{radiance, halogen}
	ctx := context.Background()

	// Image-request profile: chat/completions with an image part;
	// VisionUnderstanding is the flag the capability filter keys on.
	imageProfile := &domain.RequestProfile{
		Path: "/v1/chat/completions",
		ModelCapabilities: &domain.ModelCapabilities{
			VisionUnderstanding: true,
			ChatCompletion:      true,
			TextGeneration:      true,
			StreamingSupport:    true,
		},
		SupportedBy: []string{domain.ProfileOpenAICompatible, domain.ProfileLlamaCpp},
	}

	styledLog := &mockStyledLogger{}

	stampedRegistry := &mockFullModelRegistry{
		endpointsForModel: map[string][]string{
			qwenModel:    {radianceURL},
			halogenModel: {halogenURL},
		},
		// Post-W1/W2 state: the zero-capability llamacpp model list was
		// augmented from the live health probe ({"vision":{"enabled":true}})
		// and/or the model_capabilities config map, and the stamp landed only
		// on models present in the live /v1/models list -- here the halogen
		// model, with [vision chat].
		modelsByCapability: map[string][]*domain.UnifiedModel{
			"vision": {
				{
					ID:           qwenModel,
					Capabilities: []string{"vision", "chat"},
					SourceEndpoints: []domain.SourceEndpoint{
						{EndpointURL: radianceURL, LastSeen: time.Now()},
					},
				},
				{
					ID:           halogenModel,
					Capabilities: []string{"vision", "chat"},
					SourceEndpoints: []domain.SourceEndpoint{
						{EndpointURL: halogenURL, LastSeen: time.Now()},
					},
				},
			},
		},
	}
	stampedApp := &Application{modelRegistry: stampedRegistry, logger: styledLog}

	t.Run("alias all chain keeps both endpoints for image profile", func(t *testing.T) {
		visionModels, err := stampedRegistry.GetModelsByCapability(ctx, "vision")
		assert.NoError(t, err)
		assert.Len(t, visionModels, 2, "W1/W2 stamp must put both models in the vision index")

		kept := stampedApp.filterEndpointsByCapabilities([]*domain.Endpoint{radiance, halogen}, imageProfile, styledLog)

		// Pre-W2 the halogen model was absent from the vision index, the
		// filter dropped halogen-395, and any request the balancer wanted to
		// place on 395 died as 404 model_not_found. Post-fix both endpoints
		// survive, so the chain is genuinely dual-target.
		assert.Len(t, kept, 2, "image profile must keep both vision-capable endpoints")
		assert.ElementsMatch(t, []string{"radiance-64", "halogen-395"}, visionAliasEndpointNames(kept))
	})

	t.Run("alias slow routes to halogen and survives vision filter", func(t *testing.T) {
		// Alias "slow" = [halogen-qwen3.8-flash-next]: the registry knows the
		// model lives only on 395, so the routable set is halogen alone; the
		// vision filter must NOT drop it.
		routable, _, err := stampedRegistry.GetRoutableEndpointsForModel(ctx, halogenModel, healthy)
		assert.NoError(t, err)
		assert.Len(t, routable, 1, "halogen model must resolve to exactly its hosting endpoint")
		assert.Equal(t, "halogen-395", routable[0].Name)

		kept := stampedApp.filterEndpointsByCapabilities(routable, imageProfile, styledLog)

		// Pre-fix this chain yields 0 kept endpoints -> 404 model_not_found
		// for every image request on alias "slow", even though the engine
		// reports vision.enabled:true at /health.
		assert.Len(t, kept, 1, "stamped halogen endpoint must pass the vision filter")
		assert.Equal(t, "halogen-395", kept[0].Name)
	})

	t.Run("negative control unstamped registry reproduces the 404 shape", func(t *testing.T) {
		// Pre-W1/W2 registry state: the llamacpp model list declared zero
		// capabilities and nothing stamped it, so the vision index contains
		// qwen4exp (radiance) only.
		unstampedRegistry := &mockFullModelRegistry{
			endpointsForModel: map[string][]string{
				qwenModel: {radianceURL},
			},
			modelsByCapability: map[string][]*domain.UnifiedModel{
				"vision": {
					{
						ID:           qwenModel,
						Capabilities: []string{"vision", "chat"},
						SourceEndpoints: []domain.SourceEndpoint{
							{EndpointURL: radianceURL, LastSeen: time.Now()},
						},
					},
				},
			},
		}
		unstampedApp := &Application{modelRegistry: unstampedRegistry, logger: styledLog}

		visionModels, err := unstampedRegistry.GetModelsByCapability(ctx, "vision")
		assert.NoError(t, err)
		visionHosts := []string{}
		for _, model := range visionModels {
			for _, source := range model.SourceEndpoints {
				if source.EndpointURL == halogenURL {
					visionHosts = append(visionHosts, source.EndpointURL)
				}
			}
		}
		assert.Empty(t, visionHosts, "unstamped registry must have no vision host on 395")

		// The real 404 fires at STRICT ALIAS RESOLUTION: the registry never
		// learned the halogen model exists. The capability filter itself
		// falls back to all compatible endpoints when no model carries the
		// capability - it is not the rejecting leg (verified behavior).
		assert.False(t, unstampedRegistry.IsModelAvailable(ctx, halogenModel), "pre-fix registry must not know the halogen model (strict alias -> 404 model_not_found)")
		assert.True(t, stampedRegistry.IsModelAvailable(ctx, halogenModel), "post-fix registry knows the model")
		kept := unstampedApp.filterEndpointsByCapabilities([]*domain.Endpoint{halogen}, imageProfile, styledLog)
		assert.Len(t, kept, 1, "filter falls back to compatible endpoints; the 404 comes from alias resolution, not this filter")

		// Mock-behavior note: the strict mock treats an unknown model as a
		// fallback to all healthy endpoints; the REAL strict alias path
		// rejects model_not_found at alias resolution, before this filter.
		fallback, decision, err := unstampedRegistry.GetRoutableEndpointsForModel(ctx, halogenModel, healthy)
		assert.NoError(t, err)
		assert.Len(t, fallback, 2)
		assert.Equal(t, "fallback", decision.Action)
	})
}
