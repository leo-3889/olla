package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/thushan/olla/internal/core/domain"
	"github.com/thushan/olla/internal/version"
)

// canonicalCapabilityFlags maps the operator capability vocabulary (config
// model_capabilities) onto the ENGINE-declaration flags the unified registry
// canonicalises: adapter/unifier canonicalEngineCapabilities turns image->vision,
// audio->audio-transcription, audio_generation->audio-speech. Discovery stamps
// the ENGINE key so the single canonicalisation point stays in the unifier.
// The config package validates the same three names (config-side list; a unit
// test pins the two vocabularies together).
var canonicalCapabilityFlags = map[string]string{
	"vision":              "image",
	"audio-transcription": "audio",
	"audio-speech":        "audio_generation",
}

const (
	// capabilityProbeTimeout bounds the health probe independently of the
	// client-wide ModelDiscovery timeout.
	capabilityProbeTimeout = 5 * time.Second
	// capabilityProbeMaxBytes bounds the health body we will parse.
	capabilityProbeMaxBytes = 256 * 1024
)

// healthCapabilityProbe is the lenient shape read from an endpoint's health
// document. Only vision is inferred: engines advertise nothing else there
// today (halogen-flash-server: {"vision":{"enabled":true,...}}), and inventing
// flags would make the advertised list un-filterable.
type healthCapabilityProbe struct {
	Vision struct {
		Enabled bool `json:"enabled"`
	} `json:"vision"`
}

// SetCapabilityProbe enables the W1 health-probe augmentation (installed by
// the discovery service at start). Off by default: a bare client performs
// exactly the requests its caller made.
func (c *HTTPModelDiscoveryClient) SetCapabilityProbe(enabled bool) {
	c.capabilityProbe = enabled
}

// SetModelCapabilities installs the operator per-model capability declarations
// (config model_capabilities). Called once at service start, before the
// discovery loop runs; the map is read-only afterwards.
func (c *HTTPModelDiscoveryClient) SetModelCapabilities(caps map[string][]string) {
	c.modelCaps = caps
}

// augmentCapabilities enriches a freshly discovered LIVE model list before it
// reaches the registry.
//
// W1 (health probe): for an endpoint whose models declare NOTHING in their
// model entries (halogen-flash-server advertises capabilities:null), probe the
// endpoint's health document; vision.enabled stamps image=true on the models
// in THIS live list only. A model that is not resident is not in the list, so
// it cannot be branded; a future resident earns its truth on its own cycle.
// The condition self-retires as soon as an engine starts declaring caps.
//
// W2 (operator declarations): config model_capabilities entries apply to the
// EXACT model id on the LIVE list - the declaration path for engines that
// advertise nothing anywhere. Declarations win over probe/engine values.
func (c *HTTPModelDiscoveryClient) augmentCapabilities(ctx context.Context, endpoint *domain.Endpoint, models []*domain.ModelInfo) {
	if len(models) == 0 {
		return
	}

	declared := false
	for _, m := range models {
		if m.Details != nil && len(m.Details.Capabilities) > 0 {
			declared = true
			break
		}
	}
	if !declared && c.capabilityProbe && endpoint.HealthCheckURLString != "" {
		if c.probeHealthVision(ctx, endpoint) {
			for _, m := range models {
				stampCapability(m, "image")
			}
			c.logger.Debug("Vision capability inferred from health probe",
				"endpoint", endpoint.Name, "models", len(models))
		}
	}

	if len(c.modelCaps) == 0 {
		return
	}
	for _, m := range models {
		for _, name := range c.modelCaps[m.Name] {
			flag, ok := canonicalCapabilityFlags[name]
			if !ok {
				// config validation rejects unknown names at boot; defensive.
				continue
			}
			stampCapability(m, flag)
		}
	}
}

func (c *HTTPModelDiscoveryClient) probeHealthVision(ctx context.Context, endpoint *domain.Endpoint) bool {
	probeCtx, cancel := context.WithTimeout(ctx, capabilityProbeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, endpoint.HealthCheckURLString, http.NoBody)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", fmt.Sprintf(DefaultUserAgent, version.ShortName, version.Version))
	req.Header.Set("Accept", DefaultContentType)
	// Same auth treatment as the model-list request: health behind a header.
	for name, value := range endpoint.Headers {
		req.Header.Set(name, value)
	}
	if endpoint.AuthHeaderName != "" {
		req.Header.Set(endpoint.AuthHeaderName, endpoint.AuthHeaderValue)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}

	var probe healthCapabilityProbe
	if err := json.NewDecoder(io.LimitReader(resp.Body, capabilityProbeMaxBytes)).Decode(&probe); err != nil {
		return false
	}
	return probe.Vision.Enabled
}

func stampCapability(m *domain.ModelInfo, flag string) {
	if m.Details == nil {
		m.Details = &domain.ModelDetails{}
	}
	if m.Details.Capabilities == nil {
		m.Details.Capabilities = make(map[string]bool, 1)
	}
	m.Details.Capabilities[flag] = true
}
