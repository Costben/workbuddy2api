package upstream

// normalizeCacheUsage replaces a zero placeholder with an observed cache hit.
// Both OpenAI's details and clients that prefer the top-level field see the hit.
// Token totals and provider billing fields remain unchanged.
func normalizeCacheUsage(u map[string]any) map[string]any {
	if u == nil {
		return nil
	}
	positive := func(v any) float64 {
		n, ok := num64(v)
		if !ok || n <= 0 {
			return 0
		}
		return n
	}
	details, _ := u["prompt_tokens_details"].(map[string]any)
	cached := positive(u["cached_tokens"])
	if cached == 0 {
		cached = positive(u["prompt_cache_hit_tokens"])
	}
	if cached == 0 {
		cached = positive(details["cached_tokens"])
	}
	if cached == 0 {
		return u
	}
	out := make(map[string]any, len(u)+2)
	for k, v := range u {
		out[k] = v
	}
	out["cached_tokens"] = cached
	if positive(details["cached_tokens"]) == 0 {
		d := make(map[string]any, len(details)+1)
		for k, v := range details {
			d[k] = v
		}
		d["cached_tokens"] = cached
		out["prompt_tokens_details"] = d
	}
	return out
}
