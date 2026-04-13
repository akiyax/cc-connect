package core

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DedupProxy is a local reverse proxy that deduplicates concurrent
// /messages requests. Claude Code's sdk-cli mode (stdin/stdout piped)
// sends two concurrent API requests per turn. Providers with strict
// rate limits (e.g. GLM-5.1 ≈ 1 req/min) reject the second request
// with 429. This proxy allows only one in-flight /messages request at
// a time and returns a synthesized empty response for duplicates.
type DedupProxy struct {
	targetURL string
	listener  net.Listener
	server    *http.Server
	once      sync.Once

	mu        sync.Mutex
	inFlight  bool    // is a /messages request currently in-flight?
	lastDone  float64 // unix timestamp of last successful response
	cooldown  float64 // seconds to wait after last response
}

// NewDedupProxy creates and starts a local dedup reverse proxy.
// targetURL is the upstream API base URL. thinkingOverride, if non-empty,
// additionally rewrites thinking.type "adaptive" (combining both functions).
// cooldown is the minimum seconds between consecutive /messages requests.
// Returns the proxy and local URL to use as ANTHROPIC_BASE_URL.
func NewDedupProxy(targetURL, thinkingOverride string, cooldown float64) (*DedupProxy, string, error) {
	target, err := url.Parse(strings.TrimRight(targetURL, "/"))
	if err != nil {
		return nil, "", fmt.Errorf("dedupproxy: parse target: %w", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", fmt.Errorf("dedupproxy: listen: %w", err)
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	origDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		origDirector(req)
		req.Host = target.Host
	}
	proxy.FlushInterval = -1

	dp := &DedupProxy{
		targetURL: targetURL,
		listener:  listener,
		cooldown:  cooldown,
	}

	override := thinkingOverride
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		isMessages := r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/messages")

		if isMessages {
			if dp.shouldBlock() {
				dp.writeFakeResponse(w)
				return
			}
			// Optionally rewrite thinking
			if override != "" {
				rewriteThinkingInRequest(r, override)
			}
			// Wrap ResponseWriter to detect completion
			dw := &dedupResponseWriter{ResponseWriter: w, dp: dp}
			proxy.ServeHTTP(dw, r)
			dw.finish()
			return
		}

		// Non-messages requests pass through unchanged
		if override != "" && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/messages") {
			rewriteThinkingInRequest(r, override)
		}
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

// shouldBlock returns true if the request should be blocked (duplicate).
// If allowed, marks as in-flight.
func (dp *DedupProxy) shouldBlock() bool {
	dp.mu.Lock()
	defer dp.mu.Unlock()

	now := float64(time.Now().UnixMilli()) / 1000.0

	if dp.inFlight {
		slog.Warn("dedupproxy: blocked concurrent /messages request (in-flight)")
		return true
	}

	if dp.cooldown > 0 && dp.lastDone > 0 && (now-dp.lastDone) < dp.cooldown {
		slog.Warn("dedupproxy: blocked /messages request (cooldown)",
			"elapsed", fmt.Sprintf("%.1fs", now-dp.lastDone),
			"cooldown", dp.cooldown)
		return true
	}

	dp.inFlight = true
	slog.Info("dedupproxy: allowing /messages request")
	return false
}

// markDone marks the in-flight request as completed.
func (dp *DedupProxy) markDone(status int) {
	dp.mu.Lock()
	defer dp.mu.Unlock()
	dp.inFlight = false
	if status >= 200 && status < 300 {
		dp.lastDone = float64(time.Now().UnixMilli()) / 1000.0
	}
	slog.Info("dedupproxy: /messages response", "status", status)
}

// writeFakeResponse writes a minimal valid Anthropic API response.
func (dp *DedupProxy) writeFakeResponse(w http.ResponseWriter) {
	resp := map[string]any{
		"id":            "msg_dedup_blocked",
		"type":          "message",
		"role":          "assistant",
		"model":         "dedup-blocked",
		"content":       []any{},
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  0,
			"output_tokens": 0,
		},
	}
	body, _ := json.Marshal(resp)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(body)
	slog.Info("dedupproxy: returned fake 200 for blocked request")
}

// Close shuts down the proxy.
func (dp *DedupProxy) Close() {
	dp.once.Do(func() {
		dp.server.Close()
	})
}

// dedupResponseWriter wraps http.ResponseWriter to capture the status code
// and signal completion back to the DedupProxy.
type dedupResponseWriter struct {
	http.ResponseWriter
	dp         *DedupProxy
	statusCode int
	finished   bool
}

func (dw *dedupResponseWriter) WriteHeader(code int) {
	dw.statusCode = code
	dw.ResponseWriter.WriteHeader(code)
}

func (dw *dedupResponseWriter) Flush() {
	if f, ok := dw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// finish is called after ServeHTTP returns (response fully sent to client).
func (dw *dedupResponseWriter) finish() {
	if dw.finished {
		return
	}
	dw.finished = true
	status := dw.statusCode
	if status == 0 {
		status = 200
	}
	dw.dp.markDone(status)
}
