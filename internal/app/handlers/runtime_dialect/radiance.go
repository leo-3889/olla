package runtime_dialect

func selectRadiance(values map[string]float64) Counters {
	c := Counters{
		GenerationTokens:  first(values, "radiance:decode_tokens_total"),
		GenerationSeconds: first(values, "radiance:engine_step_seconds_decode_total"),
		PrefillTokens:     first(values, "radiance:prefill_tokens_total"),
		PrefillSeconds:    first(values, "radiance:engine_step_seconds_prefill_total"),
		GenRate:           first(values, "radiance:decode_tokens_per_second"),
		PrefillRate:       first(values, "radiance:prefill_tokens_per_second"),
		KVUsage:           normalisePercent(first(values, vllmKVUsageNames...)),
		QueueDepth:        first(values, "radiance:http_queue_depth"),
		RequestsRunning:   first(values, "vllm:num_requests_running"),
		SpecAcceptPercent: scaledRatioPercent(first(values, "radiance:draft_acceptance_ratio")),
	}
	// No counter pair forms a hit rate here: attn_cached_tokens_total is
	// tokens while linear_checkpoints_written_total counts checkpoints
	// (their ratio is tokens-per-checkpoint, not a percentage), and the
	// paged-attention cache exposes no queries counter. The GPU-prefix
	// cache hit-rate gauge comes first because that is the series
	// ollametrics (and therefore Grafana) canonicalises - reading the
	// linear-cache gauge instead made the dashboard and Grafana report
	// different hit rates for the same engine. The linear-cache gauge
	// stays as fallback for builds without the GPU-prefix series.
	c.CacheRatePercent = scaledRatioPercent(first(values, "vllm:gpu_prefix_cache_hit_rate", "radiance:linear_prefix_cache_hit_rate"))
	return c
}

func init() {
	Register(Dialect{
		// Radiance exposes engine-wide batch-aggregate rates across all
		// concurrent requests, not a per-stream speed.
		Kind:         "radiance",
		EngineScoped: true,
		Select:       selectRadiance,
		Columns: []Column{
			{Key: KeyGenerationTPS, Label: "Generation", Unit: UnitTokS, Windowed: true},
			{Key: KeyPrefillTPS, Label: "Prefill", Unit: UnitTokS, Windowed: true},
			{Key: KeyCacheHitPercent, Label: "Prompt-cache hit rate", Unit: UnitPercent, Windowed: true},
			{Key: KeyKVCacheUsagePercent, Label: "KV-cache utilisation", Unit: UnitPercent},
			{Key: KeyQueueDepth, Label: "Queue depth", Unit: UnitCount},
		},
	})
}
