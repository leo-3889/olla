package runtime_dialect

// selectVLLM keeps the historical spellings of every vLLM series: the
// colon-form is V1, the underscore-form predates it and some builds still
// export only that.
func selectVLLM(values map[string]float64) Counters {
	// Older vLLM builds lack the V1-only kv_computed counter;
	// prompt_tokens_total paired with request_prefill_time_seconds_sum
	// measures whole-prompt prefill over engine prefill time (not wall
	// time), so the rate stays comparable across builds.
	return Counters{
		GenerationTokens:  first(values, "vllm:generation_tokens_total", "vllm_generation_tokens_total"),
		GenerationSeconds: first(values, "vllm:request_decode_time_seconds_sum", "vllm_request_decode_time_seconds_sum"),
		PrefillTokens:     first(values, "vllm:request_prefill_kv_computed_tokens_sum", "vllm_request_prefill_kv_computed_tokens_sum", "vllm:prompt_tokens_total", "vllm_prompt_tokens_total"),
		PrefillSeconds:    first(values, "vllm:request_prefill_time_seconds_sum", "vllm_request_prefill_time_seconds_sum"),
		CacheHits:         first(values, "vllm:prefix_cache_hits_total", "vllm_prefix_cache_hits_total"),
		CacheQueries:      first(values, "vllm:prefix_cache_queries_total", "vllm_prefix_cache_queries_total"),
		KVUsage:           normalisePercent(first(values, vllmKVUsageNames...)),
	}
}

func init() {
	Register(Dialect{
		Kind:   "vllm",
		Select: selectVLLM,
		Columns: []Column{
			{Key: KeyGenerationTPS, Label: "Generation", Unit: UnitTokS, Windowed: true},
			{Key: KeyPrefillTPS, Label: "Prefill", Unit: UnitTokS, Windowed: true},
			{Key: KeyCacheHitPercent, Label: "Prompt-cache hit rate", Unit: UnitPercent, Windowed: true},
			{Key: KeyKVCacheUsagePercent, Label: "KV-cache utilisation", Unit: UnitPercent},
		},
	})
}
