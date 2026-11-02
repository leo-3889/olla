// Package runtime_dialect owns per-backend knowledge for runtime metric
// sampling: which Prometheus series a backend publishes for each signal, how
// raw values must be interpreted, and which dashboard columns that backend
// can fill. It deliberately knows nothing about timing, windowing or the
// payload wire format - the sampler owns aggregates, the envelope owns the
// payload.
//
// Adding a backend means adding ONE file here that registers a Dialect: the
// sampler scrapes it, the envelope carries whatever it produced and the
// dashboard manifest advertises its columns without any further edits.
package runtime_dialect

// Units and column keys are wire contract: the embedded dashboard renders
// them verbatim and the envelope uses the same key vocabulary, so rename
// them only together with the dashboard.
const (
	UnitTokS    = "tok_s"
	UnitPercent = "percent"
	UnitCount   = "count"
	// UnitMs is reserved for latency-style signals (time to first token,
	// inter-token latency) a future dialect may declare; the dashboard
	// renders it as "412ms" / "1.2s".
	UnitMs = "ms"
)

const (
	KeyGenerationTPS       = "generation_tps"
	KeyPrefillTPS          = "prefill_tps"
	KeyCacheHitPercent     = "cache_hit_percent"
	KeyKVCacheUsagePercent = "kv_cache_usage_percent"
	KeyQueueDepth          = "queue_depth"
)

// Counters is the engine-neutral set of raw signals a dialect selects out of
// a Prometheus exposition. A nil field means the backend does not publish
// that signal and the derived metric stays absent from the payload.
type Counters struct {
	GenerationTokens, GenerationSeconds *float64
	PrefillTokens, PrefillSeconds       *float64
	// CacheHits/CacheQueries form a hit rate from counters (vLLM).
	CacheHits, CacheQueries *float64
	// CachedPromptTokens/ComputedPromptTokens form a hit rate from the
	// prompt-side counters (llama.cpp and forks).
	CachedPromptTokens, ComputedPromptTokens *float64
	KVUsage                                  *float64
	// Backend-computed instantaneous gauges, used for a signal whose
	// counter pair is absent from the exposition (radiance publishes native
	// rate gauges alongside - or instead of - its counters).
	GenRate, PrefillRate, CacheRatePercent *float64
	// Saturation and speculative-decode gauges.
	QueueDepth, RequestsRunning, SpecAcceptPercent *float64
}

// Column declares one dashboard column this dialect can fill.
type Column struct {
	Key      string
	Label    string
	Unit     string
	Windowed bool
}

// Dialect describes one backend family.
type Dialect struct {
	// Kind is the canonical runtime kind (config.CanonicalRuntimeKind).
	Kind string
	// EngineScoped marks whose throughput values are engine-wide batch
	// aggregates across concurrent requests rather than per-stream speed;
	// the envelope states it so the UI never guesses from the kind.
	EngineScoped bool
	// Select projects a parsed exposition into engine-neutral counters.
	Select func(values map[string]float64) Counters
	// Columns lists what backends of this kind can render, in display
	// order. Signals that render attached to a column (speculative
	// acceptance) or inside one (requests running) are not columns.
	Columns []Column
}

var registry = map[string]Dialect{}

// Register adds a dialect to the registry. Duplicate kinds or incomplete
// dialects are programming errors (every call site is an init func).
func Register(d Dialect) {
	if d.Kind == "" || d.Select == nil {
		panic("runtime_dialect: dialect needs Kind and Select")
	}
	if _, dup := registry[d.Kind]; dup {
		panic("runtime_dialect: duplicate dialect " + d.Kind)
	}
	registry[d.Kind] = d
}

// For returns the dialect registered for a canonical kind.
func For(kind string) (Dialect, bool) {
	d, ok := registry[kind]
	return d, ok
}

// Known reports whether a canonical kind has a sampling dialect. Keeping
// this the single sampling gate means config may validate a wider
// vocabulary, but an unregistered kind is always reported, never guessed.
func Known(kind string) bool {
	_, ok := registry[kind]
	return ok
}

// Select projects an exposition through one dialect. Unknown kinds sample
// nothing rather than silently falling back onto another backend.
func Select(kind string, values map[string]float64) Counters {
	d, ok := registry[kind]
	if !ok {
		return Counters{}
	}
	return d.Select(values)
}

// EngineScoped reports whether throughput from this kind must carry the
// engine-wide-aggregate caveat.
func EngineScoped(kind string) bool {
	d, ok := registry[kind]
	return ok && d.EngineScoped
}

// UnionColumns merges the column sets of the given kinds for the dashboard
// manifest: kinds are de-duplicated in sorted order so the output is stable
// across restarts, unregistered kinds contribute nothing, and the first
// dialect to declare a key owns its label.
func UnionColumns(kinds []string) []Column {
	seenKind := map[string]bool{}
	seenKey := map[string]bool{}
	var out []Column
	// Sorting the kind list (instead of registry iteration) keeps the
	// manifest byte-stable for one config.
	kinds = append([]string(nil), kinds...)
	sortStrings(kinds)
	for _, kind := range kinds {
		d, ok := registry[kind]
		if !ok || seenKind[kind] {
			continue
		}
		seenKind[kind] = true
		for _, c := range d.Columns {
			if seenKey[c.Key] {
				continue
			}
			seenKey[c.Key] = true
			out = append(out, c)
		}
	}
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// first returns the first name present in the exposition, nil if none.
func first(values map[string]float64, names ...string) *float64 {
	for _, name := range names {
		if v, ok := values[name]; ok {
			c := v
			return &c
		}
	}
	return nil
}

// normalisePercent maps a 0..1 ratio gauge to percent; values already in
// percent pass through.
func normalisePercent(v *float64) *float64 {
	if v == nil {
		return nil
	}
	n := *v
	if n >= 0 && n <= 1 {
		n *= 100
	}
	return &n
}

// scaledRatioPercent converts a documented 0..1 ratio gauge to percent.
// Unlike normalisePercent it never passes a >1 value through: these gauges
// are documented ratios, so anything else is a backend bug, not a percent.
func scaledRatioPercent(v *float64) *float64 {
	if v == nil {
		return nil
	}
	n := 100 * *v
	return &n
}

// vllmKVUsageNames chains the spellings vLLM builds have used for the KV
// cache usage gauge across releases (radiance emits the same compat names).
var vllmKVUsageNames = []string{"vllm:kv_cache_usage_ratio", "vllm_kv_cache_usage_ratio", "vllm:kv_cache_usage_perc", "vllm_kv_cache_usage_perc", "vllm:gpu_cache_usage_perc", "vllm_gpu_cache_usage_perc"}
