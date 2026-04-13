package core

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDedupProxy_AllowsSingleRequest(t *testing.T) {
	// Upstream server that returns a valid response
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":      "msg_test",
			"type":    "message",
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

func TestDedupProxy_BlocksConcurrentRequest(t *testing.T) {
	// Upstream server that takes a while to respond
	var upstreamCalls atomic.Int32
	started := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		// Signal that first request started
		select {
		case started <- struct{}{}:
		default:
		}
		// Simulate slow response
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":      "msg_test",
			"type":    "message",
			"content": []any{},
		})
	}))
	defer upstream.Close()

	dp, localURL, err := NewDedupProxy(upstream.URL, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer dp.Close()

	var wg sync.WaitGroup

	// First request — should go through
	wg.Add(1)
	var resp1Status int
	go func() {
		defer wg.Done()
		resp, err := http.Post(localURL+"/v1/messages", "application/json", nil)
		if err != nil {
			t.Error(err)
			return
		}
		resp1Status = resp.StatusCode
		resp.Body.Close()
	}()

	// Wait for first request to reach upstream
	<-started

	// Second concurrent request — should be blocked with fake 200
	resp2, err := http.Post(localURL+"/v1/messages", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()

	body, _ := io.ReadAll(resp2.Body)
	var result map[string]any
	json.Unmarshal(body, &result)

	wg.Wait()

	if resp1Status != 200 {
		t.Errorf("first request: expected 200, got %d", resp1Status)
	}
	if resp2.StatusCode != 200 {
		t.Errorf("second request: expected fake 200, got %d", resp2.StatusCode)
	}
	if result["id"] != "msg_dedup_blocked" {
		t.Errorf("expected fake response id, got %v", result["id"])
	}
	if upstreamCalls.Load() != 1 {
		t.Errorf("expected 1 upstream call, got %d", upstreamCalls.Load())
	}
}

func TestDedupProxy_CooldownBlocksQuickFollow(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":   "msg_test",
			"type": "message",
		})
	}))
	defer upstream.Close()

	dp, localURL, err := NewDedupProxy(upstream.URL, "", 1.0) // 1s cooldown
	if err != nil {
		t.Fatal(err)
	}
	defer dp.Close()

	// First request — succeeds
	resp, err := http.Post(localURL+"/v1/messages", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// Immediate second request — should be blocked by cooldown
	resp2, err := http.Post(localURL+"/v1/messages", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()

	var result map[string]any
	json.Unmarshal(body2, &result)

	if result["id"] != "msg_dedup_blocked" {
		t.Errorf("expected cooldown block, got %v", result["id"])
	}
	if upstreamCalls.Load() != 1 {
		t.Errorf("expected 1 upstream call, got %d", upstreamCalls.Load())
	}
}

func TestDedupProxy_AllowsAfterCooldown(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":   "msg_test",
			"type": "message",
		})
	}))
	defer upstream.Close()

	dp, localURL, err := NewDedupProxy(upstream.URL, "", 0.1) // 100ms cooldown
	if err != nil {
		t.Fatal(err)
	}
	defer dp.Close()

	// First request
	resp, err := http.Post(localURL+"/v1/messages", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// Wait for cooldown
	time.Sleep(150 * time.Millisecond)

	// Second request — should succeed
	resp2, err := http.Post(localURL+"/v1/messages", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()

	var result map[string]any
	json.Unmarshal(body2, &result)

	if result["id"] == "msg_dedup_blocked" {
		t.Error("request should not be blocked after cooldown")
	}
	if upstreamCalls.Load() != 2 {
		t.Errorf("expected 2 upstream calls, got %d", upstreamCalls.Load())
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

	// Multiple concurrent non-messages requests should all pass through
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
