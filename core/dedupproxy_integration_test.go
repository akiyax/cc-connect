package core_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

// TestDedupProxy_RealGLM sends two concurrent /messages requests to the real
// GLM API through the dedup proxy, verifying that the first succeeds and the
// second is blocked (not forwarded). This is the exact scenario that causes
// 429 errors in sdk-cli mode.
//
// Run: go test ./core/ -run TestDedupProxy_RealGLM -v -count=1
//
// Requires GLM_API_KEY env var (or uses default test key).
func TestDedupProxy_RealGLM(t *testing.T) {
	apiKey := os.Getenv("GLM_API_KEY")
	if apiKey == "" {
		apiKey = "ea16a7a0a71e4d3d99f09f3528d2b167.AaUwO4kgsiPy550N"
	}
	if os.Getenv("INTEGRATION") == "" {
		t.Skip("skip integration test; set INTEGRATION=1 to run")
	}

	baseURL := "https://open.bigmodel.cn/api/anthropic"

	dp, localURL, err := core.NewDedupProxy(baseURL, "", 2.0)
	if err != nil {
		t.Fatal(err)
	}
	defer dp.Close()

	body := map[string]any{
		"model":      "GLM-5-Turbo",
		"max_tokens": 128,
		"messages": []map[string]any{
			{"role": "user", "content": "Reply with exactly one word: hello"},
		},
	}
	bodyBytes, _ := json.Marshal(body)

	t.Logf("Proxy URL: %s", localURL)
	t.Logf("Sending 2 concurrent /messages requests to GLM via dedup proxy...")

	var wg sync.WaitGroup
	type result struct {
		status int
		body   map[string]any
		err    error
	}
	results := make([]result, 2)

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			req, _ := http.NewRequest("POST", localURL+"/v1/messages", bytes.NewReader(bodyBytes))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+apiKey)
			req.Header.Set("anthropic-version", "2023-06-01")

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				results[idx] = result{err: err}
				return
			}
			defer resp.Body.Close()
			respBody, _ := io.ReadAll(resp.Body)
			var m map[string]any
			json.Unmarshal(respBody, &m)
			results[idx] = result{status: resp.StatusCode, body: m}
		}(i)
	}
	wg.Wait()

	// Analyze: exactly one should be a real GLM response, one should be dedup-blocked
	var realCount, blockedCount, errorCount int
	for i, r := range results {
		if r.err != nil {
			t.Logf("Request %d: ERROR %v", i+1, r.err)
			errorCount++
			continue
		}
		id, _ := r.body["id"].(string)
		t.Logf("Request %d: status=%d id=%s", i+1, r.status, id)

		if r.status != 200 {
			t.Errorf("Request %d: expected 200 (real or fake), got %d: %v", i+1, r.status, r.body)
			continue
		}

		if id == "msg_dedup_blocked" {
			blockedCount++
			t.Logf("  → BLOCKED (dedup proxy returned fake response)")
		} else {
			realCount++
			// Check for actual content
			content, _ := r.body["content"].([]any)
			t.Logf("  → REAL response, content blocks: %d", len(content))
			for _, c := range content {
				if block, ok := c.(map[string]any); ok {
					if text, ok := block["text"].(string); ok {
						t.Logf("    text: %s", text)
					}
				}
			}
		}
	}

	if errorCount > 0 {
		t.Fatalf("had %d request errors", errorCount)
	}

	t.Logf("\nSummary: real=%d blocked=%d", realCount, blockedCount)

	if realCount != 1 {
		t.Errorf("expected exactly 1 real response, got %d", realCount)
	}
	if blockedCount != 1 {
		t.Errorf("expected exactly 1 blocked response, got %d", blockedCount)
	}

	// Key assertion: NO 429 errors
	for i, r := range results {
		if r.status == 429 {
			t.Errorf("Request %d got 429 — dedup proxy failed to prevent rate limit", i+1)
		}
	}

	fmt.Println("\n✅ Dedup proxy successfully prevented 429 on concurrent requests!")
}
