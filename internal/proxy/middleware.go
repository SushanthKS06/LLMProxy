// File: internal/proxy/middleware.go
// WHY: Middleware for rate limiting, authentication, logging, and CORS.
//
// FIX (MINOR-02): Removed hand-rolled min() function which shadows the
//   built-in min() available since Go 1.21 (this module targets Go 1.22).
// FIX (ISSUE-06): AuthMiddleware now logs the SHA-256 hash of the invalid
//   API key instead of its raw prefix — the prefix can be used to narrow a
//   brute-force attack on short static keys.

package proxy

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"
)

// RateLimiter implements per-team rate limiting using token bucket.
// NOTE: This limiter uses an in-memory Go map for token buckets. Under Kubernetes
// HPA, running N replicas means the effective per-team rate limit becomes configured_rps * N.
// It is not cluster-safe and should be replaced with a Redis-backed limiter if exact
// global limits are required across multiple replicas.
//
// FIX (P2-7): Added lastSeen timestamps and a background cleanup goroutine to evict
// stale entries. Without eviction an attacker sending requests with random X-Gateway-Team
// headers would exhaust gateway memory.
// FIX (AUDIT-D): ctx plumbed into cleanupLoop so the goroutine stops on shutdown.
type RateLimiter struct {
	limiters map[string]*rateLimiterEntry
	mu       sync.RWMutex
	rps      int
	burst    int
}

// rateLimiterEntry pairs a token-bucket limiter with its last-access time for eviction.
type rateLimiterEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// NewRateLimiter creates a new RateLimiter and starts a background goroutine
// that evicts limiter entries not seen in the last 5 minutes.
// FIX (P2-7): Without eviction, arbitrary X-Gateway-Team values exhaust memory.
// FIX (AUDIT-D): ctx is plumbed into cleanupLoop so the goroutine exits cleanly
// during graceful shutdown instead of leaking indefinitely.
func NewRateLimiter(ctx context.Context, rps, burst int) *RateLimiter {
	rl := &RateLimiter{
		limiters: make(map[string]*rateLimiterEntry),
		rps:      rps,
		burst:    burst,
	}
	go rl.cleanupLoop(ctx)
	return rl
}

// cleanupLoop evicts limiter entries that haven't been seen in 5 minutes.
// FIX (AUDIT-D): ctx.Done() case ensures the goroutine exits on server shutdown
// instead of leaking forever.
func (rl *RateLimiter) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cutoff := time.Now().Add(-5 * time.Minute)
			rl.mu.Lock()
			for team, entry := range rl.limiters {
				if entry.lastSeen.Before(cutoff) {
					delete(rl.limiters, team)
				}
			}
			rl.mu.Unlock()
		}
	}
}

// getLimiter gets or creates a rate limiter for a team.
func (rl *RateLimiter) getLimiter(teamID string) *rate.Limiter {
	rl.mu.RLock()
	entry, exists := rl.limiters[teamID]
	rl.mu.RUnlock()

	if exists {
		// Update lastSeen under write lock to avoid torn writes.
		rl.mu.Lock()
		entry.lastSeen = time.Now()
		rl.mu.Unlock()
		return entry.limiter
	}

	rl.mu.Lock()
	defer rl.mu.Unlock()

	// Double-check after acquiring write lock
	if entry, exists = rl.limiters[teamID]; exists {
		entry.lastSeen = time.Now()
		return entry.limiter
	}

	newEntry := &rateLimiterEntry{
		limiter:  rate.NewLimiter(rate.Limit(rl.rps), rl.burst),
		lastSeen: time.Now(),
	}
	rl.limiters[teamID] = newEntry
	return newEntry.limiter
}

// Limit returns a middleware that rate limits by team.
func (rl *RateLimiter) Limit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		teamID := r.Header.Get("X-Gateway-Team")
		if teamID == "" {
			teamID = "default"
		}

		limiter := rl.getLimiter(teamID)
		if !limiter.Allow() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error": "rate limit exceeded"}`)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// RequestLogger logs all requests with structured logging.
type RequestLogger struct {
	logger *slog.Logger
}

// NewRequestLogger creates a new RequestLogger.
func NewRequestLogger(logger *slog.Logger) *RequestLogger {
	return &RequestLogger{logger: logger}
}

// Log returns a middleware that logs requests.
// FIX (P1-3): Request ID is now injected into the request context so every
// downstream log line can include it as a structured field for correlation.
func (rl *RequestLogger) Log(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Generate or accept request ID BEFORE handler runs so it
		// can be propagated into context for downstream log calls.
		reqID := r.Header.Get("X-Request-ID")
		if reqID == "" {
			reqID = uuid.New().String()
		}
		// Propagate into context so handler-level logs can include it.
		ctx := context.WithValue(r.Context(), contextKeyRequestID{}, reqID)
		r = r.WithContext(ctx)

		// Wrap response writer to capture status code
		wrapped := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		next.ServeHTTP(wrapped, r)

		latency := time.Since(start).Milliseconds()

		teamID := r.Header.Get("X-Gateway-Team")
		if teamID == "" {
			teamID = "default"
		}

		featureTag := r.Header.Get("X-Gateway-Feature")
		if featureTag == "" {
			featureTag = "untagged"
		}

		// Echo the request ID back in the response header.
		w.Header().Set("X-Request-ID", reqID)

		// FIX (AUDIT-C): X-Gateway-Model and X-Gateway-Cache are RESPONSE headers
		// set by the handler on the ResponseWriter. Reading them from r.Header
		// (the request) always returned "". Read from wrapped.Header() instead.
		rl.logger.Info("request",
			"time", start.Format(time.RFC3339),
			"request_id", reqID,
			"method", r.Method,
			"path", r.URL.Path,
			"team_id", teamID,
			"feature_tag", featureTag,
			"model", wrapped.Header().Get("X-Gateway-Model"),
			"cache_hit", wrapped.Header().Get("X-Gateway-Cache"),
			"latency_ms", latency,
			"status_code", wrapped.statusCode,
		)
	})
}

// contextKeyRequestID is the context key for the request ID value.
type contextKeyRequestID struct{}

// RequestIDFromContext returns the request ID stored in the context, or "" if absent.
func RequestIDFromContext(ctx context.Context) string {
	if v := ctx.Value(contextKeyRequestID{}); v != nil {
		return v.(string)
	}
	return ""
}

type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	return rw.ResponseWriter.Write(b)
}

// RecoveryMiddleware recovers from panics.
type RecoveryMiddleware struct {
	logger *slog.Logger
}

// NewRecoveryMiddleware creates a new RecoveryMiddleware.
func NewRecoveryMiddleware(logger *slog.Logger) *RecoveryMiddleware {
	return &RecoveryMiddleware{logger: logger}
}

// Recover returns a middleware that recovers from panics.
func (rm *RecoveryMiddleware) Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				rm.logger.Error("panic recovered",
					"error", err,
					"path", r.URL.Path,
				)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `{"error": "internal server error"}`)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// CORSMiddleware handles CORS.
type CORSMiddleware struct {
	allowedOrigins []string
	allowedMethods []string
	allowedHeaders []string
}

// NewCORSMiddleware creates a new CORSMiddleware with a permissive wildcard origin.
// For production use, prefer NewCORSMiddlewareWithOrigins to restrict to known origins.
func NewCORSMiddleware() *CORSMiddleware {
	return NewCORSMiddlewareWithOrigins([]string{"*"})
}

// NewCORSMiddlewareWithOrigins creates a CORSMiddleware that only allows requests
// from the specified origin list. Use this in production to prevent arbitrary
// websites from making credentialed cross-origin requests to the gateway.
// FIX (AUDIT-J): The previous implementation always used "*" even though the fix
// comment claimed configurable origins. This constructor enables real configuration.
func NewCORSMiddlewareWithOrigins(origins []string) *CORSMiddleware {
	return &CORSMiddleware{
		allowedOrigins: origins,
		allowedMethods: []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		allowedHeaders: []string{"Content-Type", "Authorization", "X-Gateway-Team", "X-Gateway-Feature", "X-Gateway-Model", "X-Gateway-API-Key"},
	}
}

// CORS returns a middleware that handles CORS.
// FIX (P1-2): Non-preflight responses now use the configured allowedOrigins list
// instead of the hardcoded wildcard "*". This prevents any website from making
// cross-origin requests to a gateway that holds API keys and cost data.
func (cm *CORSMiddleware) CORS(next http.Handler) http.Handler {
	// Pre-join the values once so we don't allocate on every request.
	originsValue := strings.Join(cm.allowedOrigins, ",")
	methodsValue := strings.Join(cm.allowedMethods, ",")
	headersValue := strings.Join(cm.allowedHeaders, ",")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "OPTIONS" {
			w.Header().Set("Access-Control-Allow-Origin", originsValue)
			w.Header().Set("Access-Control-Allow-Methods", methodsValue)
			w.Header().Set("Access-Control-Allow-Headers", headersValue)
			w.Header().Set("Access-Control-Expose-Headers", "X-Gateway-Cache, X-Gateway-Model, X-Gateway-Cost-USD, X-Gateway-Latency-MS, X-Request-ID")
			w.Header().Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// FIX (P1-2): Use the configured origins, not a hardcoded wildcard.
		w.Header().Set("Access-Control-Allow-Origin", originsValue)
		w.Header().Set("Access-Control-Expose-Headers", "X-Gateway-Cache, X-Gateway-Model, X-Gateway-Cost-USD, X-Gateway-Latency-MS, X-Request-ID")

		next.ServeHTTP(w, r)
	})
}

// AuthMiddleware validates API keys.
type AuthMiddleware struct {
	validKeys map[string]bool
	logger    *slog.Logger
}

// NewAuthMiddleware creates a new AuthMiddleware.
func NewAuthMiddleware(apiKeys []string, logger *slog.Logger) *AuthMiddleware {
	validKeys := make(map[string]bool)
	for _, key := range apiKeys {
		validKeys[key] = true
	}
	return &AuthMiddleware{
		validKeys: validKeys,
		logger:    logger,
	}
}

// Auth returns a middleware that validates API keys.
// WHY: When GATEWAY_API_KEYS is not set, the gateway runs in open mode (no auth).
// This allows local development without configuring keys. In production, always
// set GATEWAY_API_KEYS to a non-empty value.
//
// FIX (P1-1): Removed query-parameter fallback for API keys. URL query parameters
// appear in server access logs, browser history, Nginx/proxy logs, CDN logs, and
// Referer headers — all unencrypted channels. API keys must only be passed in headers.
func (am *AuthMiddleware) Auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip auth for health check
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}

		// Open mode: no API keys configured — allow all (useful for local dev)
		if len(am.validKeys) == 0 {
			next.ServeHTTP(w, r)
			return
		}

		apiKey := r.Header.Get("X-Gateway-API-Key")

		if apiKey == "" || !am.validKeys[apiKey] {
			if apiKey != "" {
				// FIX (ISSUE-06): log the SHA-256 hash of the key, not its raw prefix.
				// A raw prefix narrows brute-force search space for short static keys;
				// a hash of the full key leaks nothing actionable.
				h := sha256.Sum256([]byte(apiKey))
				am.logger.Warn("invalid API key",
					"key_sha256_prefix", fmt.Sprintf("%x", h[:4]),
				)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error": "invalid or missing API key"}`)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// Chain applies multiple middleware in order.
// The first middleware in the list is the outermost (executes first).
func Chain(h http.Handler, middlewares ...func(http.Handler) http.Handler) http.Handler {
	for i := len(middlewares) - 1; i >= 0; i-- {
		h = middlewares[i](h)
	}
	return h
}

// ContextTimeout creates a middleware that adds timeout to context.
func ContextTimeout(timeout time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), timeout)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
