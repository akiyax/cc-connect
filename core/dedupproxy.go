package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DedupProxy is a local reverse proxy that deduplicates concurrent /messages
// requests.
//
// Claude Code's sdk-cli mode sends two concurrent (hedged) API requests per
// turn. Providers with strict rate limits (e.g. GLM-5.1) reject the second
// request with 429.
//
// Strategy:
//   - Only one /messages request at a time (serialized via mutex).
//   - After each response, enforce a cooldown before the next request.
//   - If a second request arrives during cooldown (same-turn hedged duplicate),
//     the proxy hijacks the TCP connection and closes it immediately. This
//     causes a connection-reset error on the client side. Claude Code's hedging
//     logic sees Request 1 succeed and Request 2 fail with a network error,
//     and uses Request 1's response — exactly like interactive mode.
//   - If the upstream returns 429, the proxy retries with exponential backoff.
//   - On Close(), all in-flight requests are cancelled immediately via context.
type DedupProxy struct {
	targetURL string
	listener  net.Listener
	server    *http.Server
	once      sync.Once
	proxyCtx  context.Context    // cancelled when Close() is called
	cancel    context.CancelFunc // cancels proxyCtx

	mu             sync.Mutex
	inFlight       bool          // is a /messages request currently being proxied?
	doneCh         chan struct{}  // closed when inFlight transitions to false
	lastDone       time.Time     // when the last response completed
	cooldown       time.Duration // minimum gap between consecutive /messages requests
	retryBackoff   time.Duration // initial backoff for 429 retries (default 5s)
}

// NewDedupProxy creates and starts a local dedup reverse proxy.
func NewDedupProxy(targetURL, thinkingOverride string, cooldown float64) (*DedupProxy, string, error) {
	target, err := url.Parse(strings.TrimRight(targetURL, "/"))
	if err != nil {
		return nil, "", fmt.Errorf("dedupproxy: parse target: %w", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", fmt.Errorf("dedupproxy: listen: %w", err)
	}

	// Standard reverse proxy for non-messages requests.
	proxy := httputil.NewSingleHostReverseProxy(target)
	origDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		origDirector(req)
		req.Host = target.Host
	}
	proxy.FlushInterval = -1

	proxyCtx, cancel := context.WithCancel(context.Background())
	dp := &DedupProxy{
		targetURL:    targetURL,
		listener:     listener,
		cooldown:     time.Duration(cooldown * float64(time.Second)),
		retryBackoff: 5 * time.Second,
		proxyCtx:     proxyCtx,
		cancel:       cancel,
	}

	override := thinkingOverride
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		isMessages := r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/messages")

		if isMessages {
			// Merge request context with proxy lifecycle context so that
			// Close() cancels in-flight retries immediately.
			ctx, ctxCancel := mergeContexts(r.Context(), dp.proxyCtx)
			defer ctxCancel()

			acquired, wasQueued := dp.acquire(ctx)
			if wasQueued {
				// Same-turn hedged duplicate: hijack the TCP connection and
				// close it immediately. Claude Code sees a network error for
				// this request and uses Request 1's successful response.
				slog.Info("dedupproxy: killing duplicate request (TCP reset)")
				if hj, ok := w.(http.Hijacker); ok {
					if conn, _, err := hj.Hijack(); err == nil {
						conn.Close()
					}
				}
				return
			}
			if !acquired {
				slog.Info("dedupproxy: /messages request cancelled")
				return
			}

			if override != "" {
				rewriteThinkingInRequest(r, override)
			}

			// Read the request body so we can replay it on retry.
			bodyBytes, err := io.ReadAll(r.Body)
			r.Body.Close()
			if err != nil {
				slog.Error("dedupproxy: read request body", "error", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				dp.release(500)
				return
			}

			// Log request details for debugging.
			hdrs := make(map[string]string)
			for k := range r.Header {
				if strings.EqualFold(k, "Authorization") || strings.EqualFold(k, "X-Api-Key") {
					hdrs[k] = "[REDACTED]"
				} else {
					hdrs[k] = r.Header.Get(k)
				}
			}
			slog.Info("dedupproxy: request details",
				"method", r.Method,
				"path", r.URL.Path,
				"headers", hdrs,
				"body_len", len(bodyBytes),
				"body_preview", truncate(string(bodyBytes), 2000),
			)

			// Sanitize the request body for GLM compatibility.
			bodyBytes = sanitizeRequestBody(bodyBytes)

			// Forward to upstream.
			dp.forwardRequest(ctx, w, r, bodyBytes, target)
			return
		}

		// Non-messages requests pass through unchanged.
		proxy.ServeHTTP(w, r)
	})

	dp.server = &http.Server{
		Handler:      mux,
		ReadTimeout:  10 * time.Minute,
		WriteTimeout: 10 * time.Minute,
	}

	go func() {
		if err := dp.server.Serve(listener); err != nil && err != http.ErrServerClosed {
			slog.Error("dedupproxy: serve error", "error", err)
		}
	}()

	localURL := fmt.Sprintf("http://127.0.0.1:%d", listener.Addr().(*net.TCPAddr).Port)
	slog.Info("dedupproxy: started", "target", targetURL, "local", localURL,
		"thinking", thinkingOverride, "cooldown", cooldown)
	return dp, localURL, nil
}

// forwardRequest sends the request to upstream. On 429, it retries with
// exponential backoff (5s, 10s, 20s) up to 3 times. If all retries fail,
// the proxy returns a fake 200 with an error message so Claude Code won't
// retry internally and the user sees the error in the chat.
func (dp *DedupProxy) forwardRequest(ctx context.Context, w http.ResponseWriter, origReq *http.Request, body []byte, target *url.URL) {
	upstreamURL := target.JoinPath(origReq.URL.Path).String()
	if origReq.URL.RawQuery != "" {
		upstreamURL += "?" + origReq.URL.RawQuery
	}

	const maxRetries = 3
	backoff := dp.retryBackoff

	for attempt := 0; attempt <= maxRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, origReq.Method, upstreamURL, bytes.NewReader(body))
		if err != nil {
			slog.Error("dedupproxy: create request", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			dp.release(500)
			return
		}
		for k, vv := range origReq.Header {
			for _, v := range vv {
				req.Header.Add(k, v)
			}
		}
		req.ContentLength = int64(len(body))

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				slog.Info("dedupproxy: request cancelled")
				dp.release(0)
				return
			}
			slog.Error("dedupproxy: upstream request failed", "error", err)
			http.Error(w, "upstream error", http.StatusBadGateway)
			dp.release(502)
			return
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			rateLimitBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			resp.Body.Close()

			if attempt < maxRetries {
				slog.Warn("dedupproxy: upstream 429, retrying", "attempt", attempt+1, "backoff", backoff, "body", string(rateLimitBody))
				select {
				case <-time.After(backoff):
					backoff *= 2
					continue
				case <-ctx.Done():
					slog.Info("dedupproxy: retry cancelled")
					dp.release(0)
					return
				}
			}

			// All retries exhausted.
			slog.Error("dedupproxy: upstream 429 after all retries", "body", string(rateLimitBody))
			msg := fmt.Sprintf("⚠️ 429 Rate Limited (after %d retries)\n\n%s", maxRetries, string(rateLimitBody))
			dp.writeErrorAsMessage(w, msg)
			dp.release(429)
			return
		}

		// Read the response body to check for GLM's fake-200 rate limit.
		// GLM sometimes returns HTTP 200 with "event: error" SSE containing
		// rate limit code 1302 — which is effectively a 429 wrapped in 200.
		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			slog.Error("dedupproxy: read response body", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			dp.release(500)
			return
		}

		// Log response details for debugging.
		respHdrs := make(map[string]string)
		for k := range resp.Header {
			respHdrs[k] = resp.Header.Get(k)
		}
		slog.Info("dedupproxy: upstream response",
			"status", resp.StatusCode,
			"headers", respHdrs,
			"body_len", len(respBody),
			"body_preview", truncate(string(respBody), 2000),
		)

		// Detect GLM fake-200 rate limit: HTTP 200 but body starts with
		// "event: error" and contains rate limit code.
		if resp.StatusCode == http.StatusOK && isGLMFake200RateLimit(respBody) {
			slog.Warn("dedupproxy: detected GLM fake-200 rate limit, treating as 429",
				"attempt", attempt+1, "backoff", backoff)

			if attempt < maxRetries {
				select {
				case <-time.After(backoff):
					backoff *= 2
					continue
				case <-ctx.Done():
					slog.Info("dedupproxy: retry cancelled")
					dp.release(0)
					return
				}
			}

			// All retries exhausted.
			slog.Error("dedupproxy: GLM fake-200 rate limit after all retries")
			msg := fmt.Sprintf("⚠️ 429 Rate Limited (GLM returned 200 with error, after %d retries)", maxRetries)
			dp.writeErrorAsMessage(w, msg)
			dp.release(429)
			return
		}

		// Real success — write the buffered response back to the client.
		for k, vv := range resp.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		w.Write(respBody)

		dp.release(resp.StatusCode)
		return
	}
}

// acquire tries to claim the serialization slot.
//
// Returns:
//   - (true, false)  — slot acquired, proceed with upstream request
//   - (false, true)  — same-turn duplicate detected (during cooldown or in-flight),
//     caller should kill the connection immediately
//   - (false, false) — context cancelled, caller should return silently
func (dp *DedupProxy) acquire(ctx context.Context) (acquired bool, wasQueued bool) {
	dp.mu.Lock()
	if !dp.inFlight {
		// Check cooldown. Any request arriving during cooldown is the
		// hedged duplicate from the same Claude Code turn.
		if dp.cooldown > 0 && !dp.lastDone.IsZero() {
			if time.Since(dp.lastDone) < dp.cooldown {
				dp.mu.Unlock()
				slog.Info("dedupproxy: request during cooldown (same-turn duplicate)")
				return false, true
			}
		}
		dp.inFlight = true
		dp.doneCh = make(chan struct{})
		dp.mu.Unlock()
		slog.Info("dedupproxy: acquired slot for /messages request")
		return true, false
	}

	// Slot is busy — this is either a hedged duplicate or a queued request.
	// Wait briefly: if the slot frees within cooldown, it's a hedged dup.
	ch := dp.doneCh
	dp.mu.Unlock()

	slog.Info("dedupproxy: queuing /messages request (slot busy)")
	select {
	case <-ch:
		// Slot freed. Now in cooldown — this is the hedged duplicate.
		slog.Info("dedupproxy: queued request is same-turn duplicate")
		return false, true
	case <-ctx.Done():
		return false, false
	}
}

// release marks the in-flight request as completed and notifies waiters.
func (dp *DedupProxy) release(status int) {
	dp.mu.Lock()
	defer dp.mu.Unlock()
	dp.inFlight = false
	dp.lastDone = time.Now()
	if dp.doneCh != nil {
		close(dp.doneCh)
		dp.doneCh = nil
	}
	slog.Info("dedupproxy: released slot", "status", status)
}

// Close shuts down the proxy and cancels all in-flight retries.
func (dp *DedupProxy) Close() {
	dp.once.Do(func() {
		dp.cancel() // cancel in-flight retry goroutines first
		dp.server.Close()
	})
}

// writeErrorAsMessage writes a fake 200 Anthropic streaming response with the
// error details as text content so Claude Code won't retry.
func (dp *DedupProxy) writeErrorAsMessage(w http.ResponseWriter, msg string) {
	dp.writeStreamingResponse(w, msg)
}

// writeStreamingResponse writes a fake 200 in SSE (text/event-stream) format
// that mimics a complete Anthropic streaming response with the given text.
func (dp *DedupProxy) writeStreamingResponse(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	id := "msg_dedup_synthetic"
	model := "dedup-proxy"

	// message_start — include all usage fields Anthropic normally returns
	fmt.Fprintf(w, "event: message_start\ndata: %s\n\n",
		mustJSON(map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": id, "type": "message", "role": "assistant",
				"content": []any{}, "model": model,
				"stop_reason": nil, "stop_sequence": nil,
				"usage": map[string]any{
					"input_tokens":                1,
					"output_tokens":               0,
					"cache_creation_input_tokens":  0,
					"cache_read_input_tokens":      0,
				},
			},
		}))

	// content_block_start
	fmt.Fprintf(w, "event: content_block_start\ndata: %s\n\n",
		mustJSON(map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": ""},
		}))

	// content_block_delta
	fmt.Fprintf(w, "event: content_block_delta\ndata: %s\n\n",
		mustJSON(map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": text},
		}))

	// content_block_stop
	fmt.Fprintf(w, "event: content_block_stop\ndata: %s\n\n",
		mustJSON(map[string]any{"type": "content_block_stop", "index": 0}))

	// message_delta — include input_tokens to match what Claude CLI expects
	fmt.Fprintf(w, "event: message_delta\ndata: %s\n\n",
		mustJSON(map[string]any{
			"type": "message_delta",
			"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
			"usage": map[string]any{
				"input_tokens":  1,
				"output_tokens": 1,
			},
		}))

	// message_stop
	fmt.Fprintf(w, "event: message_stop\ndata: %s\n\n",
		mustJSON(map[string]any{"type": "message_stop"}))

	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// mustJSON marshals v to a JSON string, panicking on error (should never fail
// for the simple maps we pass).
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("dedupproxy: mustJSON: %v", err))
	}
	return string(b)
}

// truncate returns s truncated to maxLen runes with "..." appended if needed.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// isGLMFake200RateLimit detects GLM's non-standard rate limit response:
// HTTP 200 with "event: error" SSE body containing code 1302.
func isGLMFake200RateLimit(body []byte) bool {
	s := string(body)
	return strings.Contains(s, "event: error") && strings.Contains(s, "1302")
}

// sanitizeRequestBody rewrites the request body to work around GLM compatibility
// issues. In particular, GLM's Anthropic-compatible endpoint returns 1302 rate
// limit errors when `system` is a list of content blocks (Anthropic's prompt
// caching format). This function:
//   - Converts `system` from list-of-blocks to a plain string
//   - Strips `cache_control` from message content blocks
//
// Returns the rewritten body, or the original body unchanged on any error.
func sanitizeRequestBody(body []byte) []byte {
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return body
	}

	modified := false

	// Convert system from list to string.
	if sysList, ok := data["system"].([]any); ok {
		var parts []string
		for _, item := range sysList {
			if block, ok := item.(map[string]any); ok {
				if text, ok := block["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		data["system"] = strings.Join(parts, "\n")
		modified = true
		slog.Debug("dedupproxy: converted system from list to string", "blocks", len(sysList))
	}

	// Strip cache_control from message content blocks.
	if msgs, ok := data["messages"].([]any); ok {
		for _, msg := range msgs {
			m, ok := msg.(map[string]any)
			if !ok {
				continue
			}
			content, ok := m["content"].([]any)
			if !ok {
				continue
			}
			for _, c := range content {
				block, ok := c.(map[string]any)
				if !ok {
					continue
				}
				if _, has := block["cache_control"]; has {
					delete(block, "cache_control")
					modified = true
				}
			}
		}
	}

	if !modified {
		return body
	}

	newBody, err := json.Marshal(data)
	if err != nil {
		return body
	}
	slog.Info("dedupproxy: sanitized request body", "original_len", len(body), "new_len", len(newBody))
	return newBody
}

// mergeContexts returns a context that is cancelled when either parent is done.
func mergeContexts(a, b context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(a)
	go func() {
		select {
		case <-b.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}
