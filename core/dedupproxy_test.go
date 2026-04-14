package core

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDedupProxy_AllowsSingleRequest(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "msg_test", "type": "message",
			"content": []any{map[string]any{"type": "text", "text": "hello"}},
		})
	}))
	defer upstream.Close()

	dp, localURL, err := NewDedupProxy(upstream.URL, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer dp.Close()

	resp, err := http.Post(localURL+"/v1/messages", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("expected 1 upstream call, got %d", upstreamCalls.Load())
	}
}

func TestDedupProxy_QueuesConcurrentRequest(t *testing.T) {
	// When request 2 arrives while request 1 is in-flight, it waits for
	// request 1 to complete, then is treated as a same-turn duplicate and
	// gets TCP-reset.
	var upstreamCalls atomic.Int32
	started := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		select {
		case started <- struct{}{}:
		default:
		}
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "msg_test", "type": "message"})
	}))
	defer upstream.Close()

	// cooldown=200ms — short enough for testing.
	dp, localURL, err := NewDedupProxy(upstream.URL, "", 0.2)
	if err != nil {
		t.Fatal(err)
	}
	defer dp.Close()

	var wg sync.WaitGroup
	var firstStatus int
	var secondErr error

	// First request.
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err := http.Post(localURL+"/v1/messages", "application/json", nil)
		if err != nil {
			t.Error(err)
			return
		}
		firstStatus = resp.StatusCode
		resp.Body.Close()
	}()

	<-started // first request reached upstream

	// Second concurrent request — should get TCP reset (same-turn dup).
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := http.Post(localURL+"/v1/messages", "application/json", nil)
		secondErr = err
	}()

	wg.Wait()

	if firstStatus != 200 {
		t.Errorf("first request: expected 200, got %d", firstStatus)
	}
	if secondErr == nil {
		t.Error("second request: expected error (TCP reset), got nil")
	} else {
		t.Logf("second request got expected error: %v", secondErr)
	}
	if upstreamCalls.Load() != 1 {
		t.Errorf("expected 1 upstream call, got %d", upstreamCalls.Load())
	}
}

func TestDedupProxy_CancelledQueuedRequest(t *testing.T) {
	// Second client cancels while waiting; upstream should only get 1 call.
	started := make(chan struct{}, 1)
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		select {
		case started <- struct{}{}:
		default:
		}
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "msg_test", "type": "message"})
	}))
	defer upstream.Close()

	dp, localURL, err := NewDedupProxy(upstream.URL, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer dp.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err := http.Post(localURL+"/v1/messages", "application/json", nil)
		if err == nil {
			resp.Body.Close()
		}
	}()

	<-started // first request in-flight

	// Second request with short timeout — cancels before slot is free.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, localURL+"/v1/messages", nil)
	_, err = http.DefaultClient.Do(req)
	if err == nil {
		t.Error("expected cancellation error for second request")
	}

	wg.Wait()

	if upstreamCalls.Load() != 1 {
		t.Errorf("expected 1 upstream call, got %d", upstreamCalls.Load())
	}
}

func TestDedupProxy_NonMessagesPassThrough(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(200)
	}))
	defer upstream.Close()

	dp, localURL, err := NewDedupProxy(upstream.URL, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer dp.Close()

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Post(localURL+"/v1/other", "application/json", nil)
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
		}()
	}
	wg.Wait()

	if upstreamCalls.Load() != 5 {
		t.Errorf("expected 5 upstream calls for non-messages, got %d", upstreamCalls.Load())
	}
}

func TestDedupProxy_CooldownDuplicate(t *testing.T) {
	// When request 2 arrives during cooldown (after request 1 completed),
	// it is a same-turn hedged duplicate and gets TCP-reset.
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_test","type":"message"}`))
	}))
	defer upstream.Close()

	// cooldown=300ms so the test is fast
	dp, localURL, err := NewDedupProxy(upstream.URL, "", 0.3)
	if err != nil {
		t.Fatal(err)
	}
	defer dp.Close()

	// First request — goes to upstream.
	resp1, err := http.Post(localURL+"/v1/messages", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp1.Body.Close()
	if resp1.StatusCode != 200 {
		t.Fatalf("first request: expected 200, got %d", resp1.StatusCode)
	}

	// Second request — arrives during cooldown, gets TCP reset (EOF).
	_, err = http.Post(localURL+"/v1/messages", "application/json", nil)
	if err == nil {
		t.Fatal("second request: expected error (TCP reset), got nil")
	}
	t.Logf("second request got expected error: %v", err)

	// Only the first request should have reached upstream.
	if upstreamCalls.Load() != 1 {
		t.Errorf("expected 1 upstream call, got %d", upstreamCalls.Load())
	}
}

func TestDedupProxy_429RetriesThenSucceeds(t *testing.T) {
	// Upstream returns 429 twice, then 200 on third attempt.
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := upstreamCalls.Add(1)
		if n <= 2 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"message":"rate limit exceeded","code":1302}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "msg_test", "type": "message"})
	}))
	defer upstream.Close()

	dp, localURL, err := NewDedupProxy(upstream.URL, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	dp.retryBackoff = 10 * time.Millisecond // fast retries for testing
	defer dp.Close()

	resp, err := http.Post(localURL+"/v1/messages", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 after retry, got %d", resp.StatusCode)
	}
	if upstreamCalls.Load() != 3 {
		t.Fatalf("expected 3 upstream calls (2 retries + 1 success), got %d", upstreamCalls.Load())
	}
}

func TestDedupProxy_429ExhaustsRetries(t *testing.T) {
	// Upstream always returns 429; after max retries proxy returns fake SSE 200.
	rateLimitMsg := `{"error":{"message":"rate limit exceeded","code":1302}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(rateLimitMsg))
	}))
	defer upstream.Close()

	dp, localURL, err := NewDedupProxy(upstream.URL, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	dp.retryBackoff = 10 * time.Millisecond // fast retries for testing
	defer dp.Close()

	resp, err := http.Post(localURL+"/v1/messages", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected fake 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("expected text/event-stream, got %s", ct)
	}

	// Parse SSE events to find the content_block_delta with error text.
	found := false
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := line[len("data: "):]
		var evt map[string]any
		if err := json.Unmarshal([]byte(data), &evt); err != nil {
			continue
		}
		if evt["type"] != "content_block_delta" {
			continue
		}
		delta, _ := evt["delta"].(map[string]any)
		text, _ := delta["text"].(string)
		if strings.Contains(text, "429") && strings.Contains(text, "rate limit exceeded") {
			found = true
		}
	}
	if !found {
		t.Error("expected SSE content_block_delta with 429 error message")
	}
}

func TestDedupProxy_GLMFake200RateLimit(t *testing.T) {
	// GLM returns HTTP 200 with "event: error" SSE body for rate limits.
	// The proxy should detect this and retry, treating it as 429.
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := upstreamCalls.Add(1)
		if n <= 2 {
			// First 2 calls: fake-200 rate limit (GLM style)
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			w.WriteHeader(200)
			fmt.Fprint(w, "event: error\ndata: {\"error\":{\"code\":\"1302\",\"message\":\"rate limit\"},\"request_id\":\"abc\"}\n\ndata: [DONE]\n\n")
			return
		}
		// Third call: success
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(200)
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
	}))
	defer upstream.Close()

	dp, localURL, err := NewDedupProxy(upstream.URL, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	dp.retryBackoff = 10 * time.Millisecond
	defer dp.Close()

	resp, err := http.Post(localURL+"/v1/messages", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "message_start") {
		t.Errorf("expected successful SSE response, got: %s", truncate(string(body), 200))
	}
	if upstreamCalls.Load() != 3 {
		t.Errorf("expected 3 upstream calls (2 fake-200 + 1 success), got %d", upstreamCalls.Load())
	}
}

func TestSanitizeRequestBody(t *testing.T) {
	t.Run("converts system list to string", func(t *testing.T) {
		input := map[string]any{
			"model": "GLM-5.1",
			"system": []any{
				map[string]any{"type": "text", "text": "You are helpful."},
				map[string]any{"type": "text", "text": "Be concise.", "cache_control": map[string]any{"type": "ephemeral"}},
			},
			"messages": []any{
				map[string]any{"role": "user", "content": "hi"},
			},
		}
		inputBytes, _ := json.Marshal(input)
		result := sanitizeRequestBody(inputBytes)

		var got map[string]any
		if err := json.Unmarshal(result, &got); err != nil {
			t.Fatal(err)
		}

		sysStr, ok := got["system"].(string)
		if !ok {
			t.Fatalf("expected system to be string, got %T", got["system"])
		}
		if sysStr != "You are helpful.\nBe concise." {
			t.Errorf("unexpected system: %q", sysStr)
		}
	})

	t.Run("strips cache_control from messages", func(t *testing.T) {
		input := map[string]any{
			"model":  "GLM-5.1",
			"system": "You are helpful.",
			"messages": []any{
				map[string]any{
					"role": "user",
					"content": []any{
						map[string]any{
							"type":          "text",
							"text":          "hello",
							"cache_control": map[string]any{"type": "ephemeral"},
						},
					},
				},
			},
		}
		inputBytes, _ := json.Marshal(input)
		result := sanitizeRequestBody(inputBytes)

		var got map[string]any
		if err := json.Unmarshal(result, &got); err != nil {
			t.Fatal(err)
		}

		msgs := got["messages"].([]any)
		content := msgs[0].(map[string]any)["content"].([]any)
		block := content[0].(map[string]any)
		if _, has := block["cache_control"]; has {
			t.Error("cache_control should have been stripped")
		}
		if block["text"] != "hello" {
			t.Errorf("text should be preserved, got %v", block["text"])
		}
	})

	t.Run("no-op for already clean body", func(t *testing.T) {
		input := map[string]any{
			"model":    "GLM-5.1",
			"system":   "You are helpful.",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}
		inputBytes, _ := json.Marshal(input)
		result := sanitizeRequestBody(inputBytes)

		// Should return original bytes unchanged (same content)
		var orig, got map[string]any
		json.Unmarshal(inputBytes, &orig)
		json.Unmarshal(result, &got)

		origJSON, _ := json.Marshal(orig)
		gotJSON, _ := json.Marshal(got)
		if string(origJSON) != string(gotJSON) {
			t.Errorf("body should be unchanged\noriginal: %s\ngot: %s", origJSON, gotJSON)
		}
	})

	t.Run("handles invalid JSON gracefully", func(t *testing.T) {
		input := []byte("not json")
		result := sanitizeRequestBody(input)
		if string(result) != string(input) {
			t.Error("should return original on invalid JSON")
		}
	})
}
