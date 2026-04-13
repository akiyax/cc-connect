package codex

import "testing"

func TestParseCodexUsage_TopLevelUsage(t *testing.T) {
	in, out := parseCodexUsage(map[string]any{
		"type": "turn.completed",
		"usage": map[string]any{
			"input_tokens":  float64(1234),
			"output_tokens": float64(567),
		},
	})
	if in != 1234 || out != 567 {
		t.Fatalf("parseCodexUsage(top-level usage) = %d/%d, want 1234/567", in, out)
	}
}

func TestParseCodexUsage_AppServerTurnUsage(t *testing.T) {
	in, out := parseCodexUsage(map[string]any{
		"threadId": "thread-1",
		"turn": map[string]any{
			"id": "turn-1",
			"usage": map[string]any{
				"prompt_tokens":     float64(42),
				"completion_tokens": float64(11),
			},
		},
	})
	if in != 42 || out != 11 {
		t.Fatalf("parseCodexUsage(turn.usage) = %d/%d, want 42/11", in, out)
	}
}

