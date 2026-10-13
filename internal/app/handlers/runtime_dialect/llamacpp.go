package runtime_dialect

func selectLLamaCpp(values map[string]float64) Counters {
	return Counters{
		GenerationTokens:     first(values, "llamacpp:tokens_predicted_total", "llamacpp_tokens_predicted_total"),
		GenerationSeconds:    first(values, "llamacpp:tokens_predicted_seconds_total", "llamacpp_tokens_predicted_seconds_total"),
		PrefillTokens:        first(values, "llamacpp:prompt_tokens_total", "llamacpp_prompt_tokens_total"),
		PrefillSeconds:       first(values, "llamacpp:prompt_seconds_total", "llamacpp_prompt_seconds_total"),
		CachedPromptTokens:   first(values, "halogen:prompt_tokens_cached_total", "halogen_prompt_tokens_cached_total"),
		ComputedPromptTokens: first(values, "llamacpp:prompt_tokens_total", "llamacpp_prompt_tokens_total"),
		KVUsage:              normalisePercent(first(values, "llamacpp:kv_cache_usage_ratio", "llamacpp_kv_cache_usage_ratio")),
	}
}

func init() {
	Register(Dialect{
		Kind:   "llamacpp",
		Select: selectLLamaCpp,
		Columns: []Column{
			{Key: KeyGenerationTPS, Label: "Generation", Unit: UnitTokS, Windowed: true},
			{Key: KeyPrefillTPS, Label: "Prefill", Unit: UnitTokS, Windowed: true},
			{Key: KeyCacheHitPercent, Label: "Prompt-cache hit rate", Unit: UnitPercent, Windowed: true},
			{Key: KeyKVCacheUsagePercent, Label: "KV-cache utilisation", Unit: UnitPercent},
		},
	})
}
