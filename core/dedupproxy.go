package core

import (
	"context"
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

// DedupProxy is a local reverse proxy that serializes concurrent /messages
// requests. Claude Code's sdk-cli mode (stdin/stdout piped) sends two
// concurrent API requests per turn. Providers with strict concurrency limits
// (e.g. GLM-5.1 ≈ 1 concurrent request) reject the second with 429.
//
// Instead of returning a fake response for the duplicate, we queue it and
// forward it only after the first request completes. Claude Code typically
// cancels the second request once it has received the first response, so in
// practice the queued request is never actually forwarded.
type DedupProxy struct {
	targetURL string
	listener  net.Listener
	server    *http.Server
	once      sync.Once

	mu       sync.Mutex
	inFlight bool          // is a /messages request currently being proxied?
	doneCh   chan struct{}  // closed when inFlight transitions to false
}

// NewDedupProxy creates and starts a local dedup reverse proxy.
// targetURL is the upstream API base URL. thinkingOverride, if non-empty,
// additionally rewrites thinking.type in the request body.
// Returns the proxy and local URL to use as ANTHROPIC_BASE_URL.
func NewDedupProxy(targetURL, thinkingOverride string) (*DedupProxy, string, error) {
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
	}

	override := thinkingOverride
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		isMessages := r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/messages")

		if isMessages {
			// Acquire the serialization slot. If another request is already
			// in-flight, block until it completes or the client cancels.
			if !dp.acquire(r.Context()) {
				// Client cancelled (e.g. Claude Code already got a response
				// from the first request and dropped this connection).
				slog.Info("dedupproxy: queued /messages request cancelled by client")
				return
			}
			if override != "" {
				rewriteThinkingInRequest(r, override)
			}
			// defer ensures the slot is always released even on panic/error.
			dw := &dedupResponseWriter{ResponseWriter: w, dp: dp}
			defer dw.finish()
			proxy.ServeHTTP(dw, r)
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
		"thinking", thinkingOverride)
	return dp, localURL, nil
}

// acquire blocks until the serialization slot is free, then claims it.
// Returns false if ctx is cancelled before the slot becomes available.
func (dp *DedupProxy) acquire(ctx context.Context) bool {
	for {
		dp.mu.Lock()
		if !dp.inFlight {
			dp.inFlight = true
			dp.doneCh = make(chan struct{})
			dp.mu.Unlock()
			slog.Info("dedupproxy: acquired slot for /messages request")
			return true
		}
		ch := dp.doneCh
		dp.mu.Unlock()

		slog.Info("dedupproxy: queuing /messages request (slot busy)")
		select {
		case <-ch:
			// Slot released; retry the acquire loop.
		case <-ctx.Done():
			return false
		}
	}
}

// release marks the in-flight request as completed and notifies waiters.
func (dp *DedupProxy) release(status int) {
	dp.mu.Lock()
	defer dp.mu.Unlock()
	dp.inFlight = false
	if dp.doneCh != nil {
		close(dp.doneCh)
		dp.doneCh = nil
	}
	slog.Info("dedupproxy: released slot after /messages response", "status", status)
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

// finish releases the serialization slot after the response is fully sent.
func (dw *dedupResponseWriter) finish() {
	if dw.finished {
		return
	}
	dw.finished = true
	status := dw.statusCode
	if status == 0 {
		status = 200
	}
	dw.dp.release(status)
}
