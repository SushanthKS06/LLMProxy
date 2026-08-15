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
type RateLimiter struct {
	limiters map[string]*rate.Limiter
	mu       sync.RWMutex
	rps      int
	burst    int
}

// NewRateLimiter creates a new RateLimiter.
func NewRateLimiter(rps, burst int) *RateLimiter {
	return &RateLimiter{
		limiters: make(map[string]*rate.Limiter),
		rps:      rps,
		burst:    burst,
	}
}

// getLimiter gets or creates a rate limiter for a team.
func (rl *RateLimiter) getLimiter(teamID string) *rate.Limiter {
	rl.mu.RLock()
	limiter, exists := rl.limiters[teamID]
	rl.mu.RUnlock()

	if exists {
		return limiter
	}

	rl.mu.Lock()
	defer rl.mu.Unlock()

	// Double-check after acquiring write lock
	if limiter, exists = rl.limiters[teamID]; exists {
		return limiter
	}

	limiter = rate.NewLimiter(rate.Limit(rl.rps), rl.burst)
	rl.limiters[teamID] = limiter
	return limiter
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
func (rl *RequestLogger) Log(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

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

		reqID := r.Header.Get("X-Request-ID")
		if reqID == "" {
			reqID = uuid.New().String()
		}
		w.Header().Set("X-Request-ID", reqID)

		rl.logger.Info("request",
			"time", start.Format(time.RFC3339),
			"request_id", reqID,
			"method", r.Method,
			"path", r.URL.Path,
			"team_id", teamID,
			"feature_tag", featureTag,
			"model", r.Header.Get("X-Gateway-Model"),
			"cache_hit", r.Header.Get("X-Gateway-Cache"),
			"latency_ms", latency,
			"status_code", wrapped.statusCode,
		)
	})
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

// NewCORSMiddleware creates a new CORSMiddleware.
func NewCORSMiddleware() *CORSMiddleware {
	return &CORSMiddleware{
		allowedOrigins: []string{"*"},
		allowedMethods: []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		allowedHeaders: []string{"Content-Type", "Authorization", "X-Gateway-Team", "X-Gateway-Feature", "X-Gateway-Model", "X-Gateway-API-Key"},
	}
}

// CORS returns a middleware that handles CORS.
func (cm *CORSMiddleware) CORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "OPTIONS" {
			w.Header().Set("Access-Control-Allow-Origin", strings.Join(cm.allowedOrigins, ","))
			w.Header().Set("Access-Control-Allow-Methods", strings.Join(cm.allowedMethods, ","))
			w.Header().Set("Access-Control-Allow-Headers", strings.Join(cm.allowedHeaders, ","))
			w.Header().Set("Access-Control-Expose-Headers", "X-Gateway-Cache, X-Gateway-Model")
			w.Header().Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
			return
		}

		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Expose-Headers", "X-Gateway-Cache, X-Gateway-Model")

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
		if apiKey == "" {
			// Check query parameter as fallback
			apiKey = r.URL.Query().Get("api_key")
		}

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
