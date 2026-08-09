// File: internal/redaction/redactor.go
// WHY: Middleware that intercepts outgoing LLM requests, replaces PII with typed
// placeholders, and restores placeholders in non-streaming responses.
//
// Design decisions are documented in implementation_spec_pii_redaction.md.
// Key insight: redacting BEFORE cache lookup normalizes prompts across different
// PII instances → higher cache hit rates. Security feature doubles as perf feature.

package redaction

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// ── Prometheus metrics ────────────────────────────────────────────────────────

var (
	piiRedactedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_pii_redacted_total",
		Help: "Total PII tokens redacted, by type and team",
	}, []string{"pii_type", "team"})

	requestsWithPII = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_requests_with_pii_total",
		Help: "Requests that contained at least one PII token, by team",
	}, []string{"team"})

	redactionErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_redaction_errors_total",
		Help: "Redaction failures by cause (body_read, serialize, panic)",
	}, []string{"cause"})
)

// ── Context key ───────────────────────────────────────────────────────────────

// contextKey is a package-private type to prevent context key collisions with
// other packages that also store values in context.
type contextKey string

const redactionMapCtxKey contextKey = "redaction_map"

// ── RedactionMap ──────────────────────────────────────────────────────────────

// RedactionMap is the per-request bidirectional mapping between PII values and
// their placeholder replacements. It is stored in the request context so both
// the middleware (for response restoration) and downstream handlers (for logging)
// can access it without coupling through function parameters.
//
// Thread safety: RWMutex guards map reads/writes. The total counter uses atomic
// operations so callers can check HasRedactions() without acquiring the lock.
type RedactionMap struct {
	mu sync.RWMutex

	// valueToPlaceholder deduplicates: the same PII value always gets the same placeholder.
	// "alice@example.com" appearing 3× in a prompt → always "[EMAIL_1]", not 1, 2, 3.
	valueToPlaceholder map[string]string

	// placeholderToValue is the reverse map used for response restoration.
	placeholderToValue map[string]string

	// counts tracks how many unique values of each type have been seen this request.
	// Used to generate the N suffix in "[EMAIL_N]".
	counts map[string]int

	// total is the count of unique PII tokens found (not occurrences).
	// Atomic so HasRedactions() needs no lock.
	total int32
}

func newRedactionMap() *RedactionMap {
	return &RedactionMap{
		valueToPlaceholder: make(map[string]string),
		placeholderToValue: make(map[string]string),
		counts:             make(map[string]int),
	}
}

// internValue returns the placeholder for a PII value, creating a new one if unseen.
// Holds the write lock for the full duration so count and map updates are atomic.
func (m *RedactionMap) internValue(piiType, value string) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	if existing, found := m.valueToPlaceholder[value]; found {
		return existing // same value → same placeholder, no double-counting
	}

	m.counts[piiType]++
	placeholder := fmt.Sprintf("%s_%d", piiType, m.counts[piiType])
	m.valueToPlaceholder[value] = placeholder
	m.placeholderToValue[placeholder] = value
	atomic.AddInt32(&m.total, 1)
	return placeholder
}

// Restore replaces all placeholders in text with their original values.
// Safe to call concurrently; used by restoringResponseWriter after the handler returns.
func (m *RedactionMap) Restore(text string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for placeholder, original := range m.placeholderToValue {
		text = strings.ReplaceAll(text, "["+placeholder+"]", original)
	}
	return text
}

// HasRedactions returns true if any PII was found in the request body.
// Lock-free — safe to call on the hot path.
func (m *RedactionMap) HasRedactions() bool {
	return atomic.LoadInt32(&m.total) > 0
}

// AsJSON returns the placeholder→original map as a JSON string.
// Used to populate the X-Gateway-Redaction-Map header for streaming clients.
// Returns "" if no redactions occurred.
func (m *RedactionMap) AsJSON() string {
	if !m.HasRedactions() {
		return ""
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, err := json.Marshal(m.placeholderToValue)
	if err != nil {
		return ""
	}
	return string(b)
}

// GetRedactionMap retrieves the per-request redaction map from the context.
// Returns nil if no redaction middleware is in the chain.
func GetRedactionMap(ctx context.Context) *RedactionMap {
	if v := ctx.Value(redactionMapCtxKey); v != nil {
		return v.(*RedactionMap)
	}
	return nil
}

// ── Redactor ──────────────────────────────────────────────────────────────────

// Config controls Redactor behavior. Read from environment at startup.
type Config struct {
	// Enabled is a master switch. Set REDACTION_ENABLED=false to bypass entirely
	// (e.g. integration test environment where you want to inspect raw prompts).
	Enabled bool

	// FailClosed controls behavior when redaction fails internally.
	// false (default): log the error, pass the request through unredacted.
	// true: return HTTP 500 — use in HIPAA/SOC 2 environments.
	FailClosed bool

	// DisabledPatterns is the list of pattern Names to skip.
	// Set via REDACTION_DISABLED_PATTERNS=ipv4,phone_us
	DisabledPatterns []string
}

// Redactor is the PII detection + redaction HTTP middleware.
// Create once at startup with NewRedactor; it is safe for concurrent use.
type Redactor struct {
	patterns []PiiPattern
	logger   *slog.Logger
	cfg      Config
}

// NewRedactor creates a Redactor from DefaultPatterns, filtered by cfg.DisabledPatterns.
// Pass extra patterns to extend the default set (e.g. internal employee ID format).
func NewRedactor(logger *slog.Logger, cfg Config, extra ...PiiPattern) *Redactor {
	disabled := make(map[string]bool, len(cfg.DisabledPatterns))
	for _, name := range cfg.DisabledPatterns {
		disabled[strings.TrimSpace(name)] = true
	}

	patterns := make([]PiiPattern, 0, len(DefaultPatterns)+len(extra))
	for _, p := range DefaultPatterns {
		if !disabled[p.Name] {
			patterns = append(patterns, p)
		}
	}
	patterns = append(patterns, extra...)

	return &Redactor{patterns: patterns, logger: logger, cfg: cfg}
}

// Redact is an http.Handler middleware. It:
//  1. Reads the full request body
//  2. Parses the JSON payload
//  3. Redacts PII from all message content fields (and top-level "system" for Anthropic)
//  4. Stores the RedactionMap in the request context
//  5. Rewrites r.Body with the redacted payload so downstream handlers see clean data
//  6. Wraps the ResponseWriter to restore placeholders in non-streaming responses
//
// WHY this placement in the chain (after auth, before rate-limit):
//   - After auth: team ID is validated; metrics are attributed correctly
//   - Before cache lookup: normalized prompts → higher cache hit rates
//   - Before provider forward: PII never reaches OpenAI / Anthropic
func (rd *Redactor) Redact(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Passthrough if disabled or not a POST with a body
		if !rd.cfg.Enabled || r.Method != http.MethodPost || r.Body == nil {
			next.ServeHTTP(w, r)
			return
		}

		original, err := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err != nil {
			redactionErrors.WithLabelValues("body_read").Inc()
			rd.logger.Error("redaction: cannot read request body", "error", err)
			if rd.cfg.FailClosed {
				http.Error(w, `{"error":"redaction error"}`, http.StatusInternalServerError)
				return
			}
			// Fail open: body is gone so we can't restore it — skip redaction entirely
			next.ServeHTTP(w, r)
			return
		}

		// Parse as a generic map so we don't couple to chatCompletionRequest.
		// This makes the redactor forward-compatible with new fields and providers.
		var payload map[string]interface{}
		if err := json.Unmarshal(original, &payload); err != nil {
			// Not JSON (shouldn't happen given Content-Type checks upstream) — pass through
			r.Body = io.NopCloser(bytes.NewReader(original))
			next.ServeHTTP(w, r)
			return
		}

		rmap := newRedactionMap()

		// ── Redact "messages" array (OpenAI + Anthropic format) ───────────────
		if msgs, ok := payload["messages"].([]interface{}); ok {
			for i, msg := range msgs {
				if m, ok := msg.(map[string]interface{}); ok {
					if content, ok := m["content"].(string); ok {
						m["content"] = rd.redactText(content, rmap)
						msgs[i] = m
					}
				}
			}
			payload["messages"] = msgs
		}

		// ── Redact top-level "system" field (Anthropic Messages API) ──────────
		if system, ok := payload["system"].(string); ok && system != "" {
			payload["system"] = rd.redactText(system, rmap)
		}

		// Serialize the redacted payload back to JSON
		redacted, err := json.Marshal(payload)
		if err != nil {
			redactionErrors.WithLabelValues("serialize").Inc()
			rd.logger.Error("redaction: cannot serialize redacted payload", "error", err)
			if rd.cfg.FailClosed {
				http.Error(w, `{"error":"redaction error"}`, http.StatusInternalServerError)
				return
			}
			// Fail open: restore original body and continue
			r.Body = io.NopCloser(bytes.NewReader(original))
			next.ServeHTTP(w, r)
			return
		}

		// ── Emit metrics and structured log ───────────────────────────────────
		if rmap.HasRedactions() {
			team := r.Header.Get("X-Gateway-Team")
			if team == "" {
				team = "default"
			}
			requestsWithPII.WithLabelValues(team).Inc()

			rmap.mu.RLock()
			for piiType, count := range rmap.counts {
				piiRedactedTotal.WithLabelValues(piiType, team).Add(float64(count))
			}
			rmap.mu.RUnlock()

			rd.logger.Info("redaction: PII detected and removed from request",
				"team", team,
				"unique_tokens_redacted", atomic.LoadInt32(&rmap.total),
			)
		}

		// ── Inject redaction map into context ─────────────────────────────────
		ctx := context.WithValue(r.Context(), redactionMapCtxKey, rmap)

		// ── Rewrite request body ───────────────────────────────────────────────
		r.Body = io.NopCloser(bytes.NewReader(redacted))
		r.ContentLength = int64(len(redacted))

		// ── Wrap ResponseWriter for response-side restoration ─────────────────
		rw := &restoringResponseWriter{
			ResponseWriter: w,
			rmap:           rmap,
		}

		next.ServeHTTP(rw, r.WithContext(ctx))

		// After handler returns: flush the buffered non-streaming response
		// (no-op if streaming, no-op if nothing was buffered)
		rw.done()
	})
}

// redactText applies all patterns to text in order and returns the redacted version.
// WHY order matters: URL_CREDS runs before EMAIL so we don't partially redact
// a connection string (user part captured as email, password left intact).
func (rd *Redactor) redactText(text string, rmap *RedactionMap) string {
	for _, p := range rd.patterns {
		text = p.Regex.ReplaceAllStringFunc(text, func(match string) string {
			if p.Validator != nil && !p.Validator(match) {
				return match
			}
			placeholder := rmap.internValue(p.Tag, match)
			return "[" + placeholder + "]"
		})
	}
	return text
}

// ── restoringResponseWriter ───────────────────────────────────────────────────

// restoringResponseWriter wraps http.ResponseWriter to intercept the response body.
//
// For non-streaming responses:
//   - Buffers the entire body
//   - On done(): replaces any PII placeholders with original values, then forwards
//
// For streaming responses (detected when Flush() is called):
//   - Immediately switches to passthrough mode
//   - Sets X-Gateway-Redaction-Map header (base64-encoded JSON map) before first byte
//   - Sets X-Gateway-Redacted: true header
//   - Passes all subsequent chunks directly to the underlying writer
//
// WHY not restore streaming chunks server-side:
//
//	Placeholders can span SSE chunk boundaries. A rolling-buffer approach requires
//	~100 lines of complex state machine code. Client-side restoration via the header
//	achieves the same end result with far less complexity. See Phase 2 roadmap.
type restoringResponseWriter struct {
	http.ResponseWriter

	rmap       *RedactionMap
	buf        bytes.Buffer
	statusCode int
	streaming  bool // true once Flush() is called
	headerSent bool // true once WriteHeader was forwarded to underlying writer
}

// WriteHeader intercepts the status code. For non-streaming, we hold it until done().
// For streaming, we forward immediately after setting redaction headers.
func (rw *restoringResponseWriter) WriteHeader(code int) {
	rw.statusCode = code
	if rw.streaming {
		if !rw.headerSent {
			rw.setRedactionHeaders()
			rw.ResponseWriter.WriteHeader(code)
			rw.headerSent = true
		}
	}
}

const maxBufferBytes = 10 * 1024 * 1024 // 10MB

// Write buffers non-streaming bodies. For streaming it passes through directly.
func (rw *restoringResponseWriter) Write(b []byte) (int, error) {
	if rw.streaming {
		return rw.ResponseWriter.Write(b)
	}

	// Enforce max buffer size to prevent memory exhaustion.
	// If the buffer would exceed 10MB, we immediately fall back to streaming
	// (passthrough) mode to protect the gateway.
	if rw.buf.Len()+len(b) > maxBufferBytes {
		rw.streaming = true
		if !rw.headerSent {
			rw.setRedactionHeaders()
			if rw.statusCode != 0 {
				rw.ResponseWriter.WriteHeader(rw.statusCode)
			}
			rw.headerSent = true
		}
		if rw.buf.Len() > 0 {
			_, _ = rw.ResponseWriter.Write(rw.buf.Bytes())
			rw.buf.Reset()
		}
		return rw.ResponseWriter.Write(b)
	}

	// Non-streaming: buffer until done() is called
	return rw.buf.Write(b)
}

// Flush signals streaming mode. Drains any buffered bytes, then passes through.
// This implements http.Flusher so the handler's flusher.(ok) assertion succeeds.
func (rw *restoringResponseWriter) Flush() {
	if !rw.streaming {
		rw.streaming = true

		// Before any bytes reach the client, set the redaction headers.
		// This is the last moment we can write headers for streaming responses.
		rw.setRedactionHeaders()

		// Forward any bytes that arrived before we knew this was a stream
		if rw.statusCode != 0 && !rw.headerSent {
			rw.ResponseWriter.WriteHeader(rw.statusCode)
			rw.headerSent = true
		}
		if rw.buf.Len() > 0 {
			_, _ = rw.ResponseWriter.Write(rw.buf.Bytes())
			rw.buf.Reset()
		}
	}

	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// done is called by Redact() after next.ServeHTTP() returns.
// For non-streaming: restores placeholders in the buffered body and sends it.
// For streaming: no-op (body was already sent chunk by chunk).
func (rw *restoringResponseWriter) done() {
	if rw.streaming || rw.buf.Len() == 0 {
		return
	}

	body := rw.buf.String()

	// Only attempt restoration if redactions actually happened
	if rw.rmap.HasRedactions() {
		body = rw.rmap.Restore(body)
	}

	// Forward the (possibly restored) body with the correct status
	if rw.statusCode > 0 {
		rw.ResponseWriter.WriteHeader(rw.statusCode)
	}
	_, _ = io.WriteString(rw.ResponseWriter, body)
}

// setRedactionHeaders adds X-Gateway-Redacted and X-Gateway-Redaction-Map to the
// response so streaming clients can perform client-side placeholder restoration.
// The map is base64-encoded to keep it a single header value.
// SECURITY NOTE: only meaningful over HTTPS — headers are encrypted in transit.
func (rw *restoringResponseWriter) setRedactionHeaders() {
	if !rw.rmap.HasRedactions() {
		return
	}
	rw.Header().Set("X-Gateway-Redacted", "true")
	if mapJSON := rw.rmap.AsJSON(); mapJSON != "" {
		rw.Header().Set(
			"X-Gateway-Redaction-Map",
			base64.StdEncoding.EncodeToString([]byte(mapJSON)),
		)
	}
}
