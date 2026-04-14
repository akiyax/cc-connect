package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

	dp, localURL, err := NewDedupProxy(upstream.URL, "")
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
	// Upstream takes a short while; second request should be queued, not faked.
	var upstreamCalls atomic.Int32
	started := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		select {
		case started <- struct{}{}:
		default:
		}
		time.Sleep(150 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "msg_test", "type": "message"})
	}))
	defer upstream.Close()

	dp, localURL, err := NewDedupProxy(upstream.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	defer dp.Close()

	var wg sync.WaitGroup
	statuses := make([]int, 2)

	// First request.
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err := http.Post(localURL+"/v1/messages", "application/json", nil)
		if err != nil {
			t.Error(err)
			return
		}
		statuses[0] = resp.StatusCode
		resp.Body.Close()
	}()

	<-started // first request reached upstream

	// Second concurrent request — should be queued, get real 200 after first completes.
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err := http.Post(localURL+"/v1/messages", "application/json", nil)
		if err != nil {
			t.Error(err)
			return
		}
		statuses[1] = resp.StatusCode
		resp.Body.Close()
	}()

	wg.Wait()

	if statuses[0] != 200 {
		t.Errorf("first request: expected 200, got %d", statuses[0])
	}
	if statuses[1] != 200 {
		t.Errorf("second request: expected queued 200, got %d", statuses[1])
	}
	// Both requests went to upstream (sequentially).
	if upstreamCalls.Load() != 2 {
		t.Errorf("expected 2 upstream calls, got %d", upstreamCalls.Load())
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

	dp, localURL, err := NewDedupProxy(upstream.URL, "")
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

	dp, localURL, err := NewDedupProxy(upstream.URL, "")
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
