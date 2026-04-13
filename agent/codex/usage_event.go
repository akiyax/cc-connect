package codex

import "strings"

// parseCodexUsage extracts token usage counters from a Codex event payload.
// The payload shape can vary across codex CLI/app-server versions, so we probe
// several known locations and field aliases.
func parseCodexUsage(raw map[string]any) (inputTokens, outputTokens int) {
	// Preferred nested locations first.
	candidates := []map[string]any{
		asMap(raw["usage"]),
		asMap(raw["stats"]),
		asMap(raw["token_usage"]),
		asMap(raw["result"]),
		asMap(raw["response"]),
	}

	if turn := asMap(raw["turn"]); turn != nil {
		candidates = append(candidates,
			asMap(turn["usage"]),
			asMap(turn["stats"]),
			asMap(turn["token_usage"]),
			turn,
		)
	}
	if result := asMap(raw["result"]); result != nil {
		candidates = append(candidates,
			asMap(result["usage"]),
			asMap(result["stats"]),
			asMap(result["token_usage"]),
		)
	}
	if resp := asMap(raw["response"]); resp != nil {
		candidates = append(candidates,
			asMap(resp["usage"]),
			asMap(resp["stats"]),
			asMap(resp["token_usage"]),
		)
	}
	// Finally, top-level direct counters.
	candidates = append(candidates, raw)

	for _, m := range candidates {
		if m == nil {
			continue
		}
		in, out, ok := parseUsageFields(m)
		if ok {
			return in, out
		}
	}
	return 0, 0
}

func parseUsageFields(m map[string]any) (inputTokens, outputTokens int, ok bool) {
	inputKeys := []string{
		"input_tokens",
		"inputTokens",
		"prompt_tokens",
		"promptTokens",
		"input",
	}
	outputKeys := []string{
		"output_tokens",
		"outputTokens",
		"completion_tokens",
		"completionTokens",
		"output",
	}

	inVal, inOK := pickFirstInt(m, inputKeys)
	outVal, outOK := pickFirstInt(m, outputKeys)
	if !inOK && !outOK {
		return 0, 0, false
	}
	if inVal < 0 {
		inVal = 0
	}
	if outVal < 0 {
		outVal = 0
	}
	return inVal, outVal, true
}

func pickFirstInt(m map[string]any, keys []string) (int, bool) {
	for _, k := range keys {
		if v, exists := m[k]; exists {
			if n, ok := toInt(v); ok {
				return n, true
			}
			// Some payloads serialize numbers as strings.
			if s, ok := v.(string); ok {
				if n, ok := toInt(strings.TrimSpace(s)); ok {
					return n, true
				}
			}
		}
	}
	return 0, false
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

