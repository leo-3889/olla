package config

import (
	"fmt"
	"strings"
)

// CanonicalRuntimeKind folds the accepted spellings of the sampled backend
// kinds onto their canonical ids; "" means unrecognised. This lives here
// because it is the single vocabulary of sampleable kinds: config validation
// and the runtime-metrics sampler must never disagree about what a pinned
// kind means, and handlers is a consumer of config, not the other way round.
func CanonicalRuntimeKind(s string) string {
	switch strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(s), "-", ""), ".", "")) {
	case "vllm":
		return "vllm"
	case "halogen", "llamacpp", "llamacppserver":
		return "llamacpp"
	case "radiance":
		return "radiance"
	}
	return ""
}

// RuntimeKindWarnings lists startup warnings for endpoints that opt into
// runtime metrics but resolve to no sampleable kind. Resolution is
// explicit-first: runtime_metrics.kind wins, then the endpoint type. There is
// deliberately NO name heuristic: host-naming from one deployment ("halogen",
// "radiance") must not be baked into product code, so a backend reached
// through a generic type (openai-compatible) must pin its kind to be sampled.
func (c *Config) RuntimeKindWarnings() []string {
	if c == nil {
		return nil
	}
	var out []string
	for _, ep := range c.Discovery.Static.Endpoints {
		if ep.RuntimeMetrics == nil || !ep.RuntimeMetrics.Enabled {
			continue
		}
		if k := strings.TrimSpace(ep.RuntimeMetrics.Kind); k != "" {
			if CanonicalRuntimeKind(k) == "" {
				out = append(out, fmt.Sprintf("endpoint %q pins runtime_metrics.kind %q, which this build does not sample; the endpoint is tracked but carries no runtime metrics", ep.Name, k))
			}
			continue
		}
		if CanonicalRuntimeKind(ep.Type) == "" {
			out = append(out, fmt.Sprintf("endpoint %q enables runtime metrics, but neither type %q nor runtime_metrics.kind identifies a sampled backend kind; pin runtime_metrics.kind (vllm, llamacpp, radiance) to sample it", ep.Name, ep.Type))
		}
	}
	return out
}
