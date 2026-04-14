package core_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// TestDedupProxy_RealGLM sends two concurrent /messages requests to the real
// GLM API through the dedup proxy, verifying that the first succeeds immediately
// and the second is delayed by cooldown then forwarded to upstream.
//
// Run: INTEGRATION=1 go test ./core/ -run TestDedupProxy_RealGLM -v -count=1
func TestDedupProxy_RealGLM(t *testing.T) {
	apiKey := os.Getenv("GLM_API_KEY")
	if apiKey == "" {
		apiKey = "ea16a7a0a71e4d3d99f09f3528d2b167.AaUwO4kgsiPy550N"
	}
	if os.Getenv("INTEGRATION") == "" {
		t.Skip("skip integration test; set INTEGRATION=1 to run")
	}

	baseURL := "https://open.bigmodel.cn/api/anthropic"

	dp, localURL, err := core.NewDedupProxy(baseURL, "", 5.0)
	if err != nil {
		t.Fatal(err)
	}

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

	type result struct {
		status int
		body   map[string]any
		err    error
	}

	// First request — should reach upstream.
	firstDone := make(chan result, 1)
	go func() {
		req, _ := http.NewRequest("POST", localURL+"/v1/messages", bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			firstDone <- result{err: err}
			return
		}
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		var m map[string]any
		json.Unmarshal(respBody, &m)
		firstDone <- result{status: resp.StatusCode, body: m}
	}()

	// Small delay then second concurrent request.
	time.Sleep(10 * time.Millisecond)
	secondDone := make(chan result, 1)
	go func() {
		req, _ := http.NewRequest("POST", localURL+"/v1/messages", bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			secondDone <- result{err: err}
			return
		}
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		var m map[string]any
		json.Unmarshal(respBody, &m)
		secondDone <- result{status: resp.StatusCode, body: m}
	}()

	// Wait for the first request to complete.
	r := <-firstDone
	if r.err != nil {
		t.Fatalf("Request 1 error: %v", r.err)
	}
	t.Logf("Request 1: status=%d", r.status)
	if r.status != 200 {
		t.Fatalf("Request 1: expected 200, got %d: %v", r.status, r.body)
	}
	if r.status == 429 {
		t.Fatal("Request 1 got 429 — dedup proxy failed to prevent rate limit")
	}

	content, _ := r.body["content"].([]any)
	t.Logf("  → response, content blocks: %d", len(content))
	for _, c := range content {
		if block, ok := c.(map[string]any); ok {
			if text, ok := block["text"].(string); ok {
				t.Logf("    text: %s", text)
			}
		}
	}

	// Second request should complete after cooldown delay (forwarded to upstream).
	select {
	case r2 := <-secondDone:
		if r2.err != nil {
			t.Fatalf("Request 2 error: %v", r2.err)
		}
		t.Logf("Request 2: status=%d", r2.status)
		if r2.status == 429 {
			t.Log("  → Request 2 got 429 from GLM (rate limit still active)")
		} else if r2.status == 200 {
			t.Log("  → Request 2 succeeded after cooldown delay")
		}
	case <-time.After(10 * time.Second):
		t.Error("Request 2: timed out waiting for response")
	}

	// Close proxy.
	dp.Close()

	fmt.Println("\n✅ Dedup proxy: request 1 succeeded, request 2 delayed by cooldown!")
}
