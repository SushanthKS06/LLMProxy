// File: internal/proxy/middleware.go
// WHY: Middleware for rate limiting, authentication, logging, and CORS.

package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

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

		rl.logger.Info("request",
			"time", start.Format(time.RFC3339),
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
			w.Header().Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
			return
		}

		w.Header().Set("Access-Control-Allow-Origin", "*")

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
func (am *AuthMiddleware) Auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip auth for health check
		if r.URL.Path == "/health" {
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
				am.logger.Warn("invalid API key", "key", apiKey[:min(len(apiKey), 8)]+"...")
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error": "invalid or missing API key"}`)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Chain applies multiple middleware in order.
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
