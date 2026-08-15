// File: internal/redaction/redactor_test.go
// WHY: Tests validate both positive cases (PII is found and replaced) and
// negative cases (false positive risk — we don't want to redact legitimate text).
// This is the primary quality gate since we can't run the binary locally.

package redaction

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// newTestRedactor creates a fully-enabled Redactor with all default patterns.
func newTestRedactor() *Redactor {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return NewRedactor(logger, Config{Enabled: true, FailClosed: false})
}

// ── Pattern Tests ─────────────────────────────────────────────────────────────

func TestPatterns_Email(t *testing.T) {
	rd := newTestRedactor()
	rmap := newRedactionMap()

	cases := []struct {
		input   string
		wantTag bool
		desc    string
	}{
		{"Contact alice@example.com for help", true, "standard email"},
		{"user.name+tag@sub.domain.co.uk", true, "email with subdomains and tags"},
		{"No email here just words", false, "no email"},
		{"version 1.2.3 is released", false, "version string (false positive check)"},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			result := rd.redactText(tc.input, rmap)
			hasTag := strings.Contains(result, "[EMAIL_")
			if hasTag != tc.wantTag {
				t.Errorf("input=%q: wantTag=%v got result=%q", tc.input, tc.wantTag, result)
			}
		})
	}
}

func TestPatterns_APIKey(t *testing.T) {
	rd := newTestRedactor()
	rmap := newRedactionMap()

	cases := []struct {
		input   string
		wantTag bool
		desc    string
	}{
		{"Authorization: Bearer sk-abc123def456ghi789jkl0", true, "OpenAI key"},
		{"github token: ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ12345", true, "GitHub PAT"},
		{"stripe key pk_live_abcdefghijklmnopqrstuvwxyz", true, "Stripe public key"},
		{"not a token: short", false, "too short to be a token"},
		{"the quick brown fox jumps", false, "no token"},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			result := rd.redactText(tc.input, rmap)
			hasTag := strings.Contains(result, "[API_KEY_")
			if hasTag != tc.wantTag {
				t.Errorf("input=%q: wantTag=%v got result=%q", tc.input, tc.wantTag, result)
			}
		})
	}
}

func TestPatterns_CreditCard(t *testing.T) {
	rd := newTestRedactor()
	rmap := newRedactionMap()

	cases := []struct {
		input   string
		wantTag bool
		desc    string
	}{
		{"Card: 4111 1111 1111 1111", true, "spaced card"},
		{"Card: 4111-1111-1111-1111", true, "dashed card"},
		{"Card: 4111111111111111", true, "unspaced card"},
		{"short 1234 5678", false, "too short"},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			result := rd.redactText(tc.input, rmap)
			hasTag := strings.Contains(result, "[CREDIT_CARD_")
			if hasTag != tc.wantTag {
				t.Errorf("input=%q: wantTag=%v got result=%q", tc.input, tc.wantTag, result)
			}
		})
	}
}

func TestPatterns_URLWithCredentials(t *testing.T) {
	rd := newTestRedactor()
	rmap := newRedactionMap()

	cases := []struct {
		input   string
		wantTag bool
		desc    string
	}{
		{"DSN: postgres://gateway:s3cret@localhost:5432/db", true, "postgres DSN"},
		{"redis://:password@redis:6379/0", true, "redis with password"},
		{"https://example.com/path", false, "URL without credentials"},
		{"ftp://user@example.com", false, "no password"},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			result := rd.redactText(tc.input, rmap)
			hasTag := strings.Contains(result, "[URL_CREDS_")
			if hasTag != tc.wantTag {
				t.Errorf("input=%q: wantTag=%v got result=%q", tc.input, tc.wantTag, result)
			}
		})
	}
}

// ── Deduplication Test ────────────────────────────────────────────────────────

func TestRedactionMap_Deduplication(t *testing.T) {
	rd := newTestRedactor()
	rmap := newRedactionMap()

	// Same email appears twice → should get the same placeholder both times
	text := "Email alice@example.com or alice@example.com again"
	result := rd.redactText(text, rmap)

	// Count occurrences of [EMAIL_1]
	count := strings.Count(result, "[EMAIL_1]")
	if count != 2 {
		t.Errorf("expected [EMAIL_1] to appear twice (deduplication), got result=%q", result)
	}

	// Exactly 1 unique redaction (not 2)
	if atomic := rmap.total; atomic != 1 {
		t.Errorf("expected 1 unique redaction, got %d", atomic)
	}
}

// ── Restoration Test ──────────────────────────────────────────────────────────

func TestRedactionMap_Restore(t *testing.T) {
	rmap := newRedactionMap()
	_ = rmap.internValue("EMAIL", "alice@example.com") // → EMAIL_1
	_ = rmap.internValue("PHONE", "555-123-4567")      // → PHONE_1

	response := `I will email [EMAIL_1] and call [PHONE_1] tomorrow.`
	restored := rmap.Restore(response)
	want := `I will email alice@example.com and call 555-123-4567 tomorrow.`

	if restored != want {
		t.Errorf("restore failed\n got:  %q\n want: %q", restored, want)
	}
}

// ── Middleware Integration Tests ───────────────────────────────────────────────

// buildChatRequest creates a request body with messages.
func buildChatRequest(t *testing.T, content string) *http.Request {
	t.Helper()
	body := map[string]interface{}{
		"model": "gpt-4o",
		"messages": []map[string]interface{}{
			{"role": "user", "content": content},
		},
	}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gateway-Team", "test-team")
	return req
}

func TestRedactMiddleware_RedactsRequestBody(t *testing.T) {
	rd := newTestRedactor()

	var capturedBody []byte
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	})

	req := buildChatRequest(t, "My email is alice@example.com and card 4111-1111-1111-1111")
	rec := httptest.NewRecorder()

	rd.Redact(next).ServeHTTP(rec, req)

	// Provider never sees the original PII
	if strings.Contains(string(capturedBody), "alice@example.com") {
		t.Error("email was NOT redacted from request body sent to provider")
	}
	if strings.Contains(string(capturedBody), "4532-1234-5678-9012") {
		t.Error("credit card was NOT redacted from request body sent to provider")
	}

	// Placeholders are present
	if !strings.Contains(string(capturedBody), "[EMAIL_1]") {
		t.Errorf("expected [EMAIL_1] in redacted body, got: %s", capturedBody)
	}
}

func TestRedactMiddleware_RestoresResponseBody(t *testing.T) {
	rd := newTestRedactor()

	// Handler echoes the placeholder back (simulating LLM echoing redacted input)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"I will contact [EMAIL_1]"}}]}`))
	})

	req := buildChatRequest(t, "Contact alice@example.com please")
	rec := httptest.NewRecorder()

	rd.Redact(next).ServeHTTP(rec, req)

	respBody := rec.Body.String()

	// Client should see the original email, not the placeholder
	if !strings.Contains(respBody, "alice@example.com") {
		t.Errorf("response was not restored: client sees placeholder instead of original\nbody: %s", respBody)
	}
	if strings.Contains(respBody, "[EMAIL_1]") {
		t.Errorf("placeholder [EMAIL_1] was not restored in response body\nbody: %s", respBody)
	}
}

func TestRedactMiddleware_NoRedactionOnCleanRequest(t *testing.T) {
	rd := newTestRedactor()

	var capturedBody []byte
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})

	req := buildChatRequest(t, "What is the capital of France?")
	rec := httptest.NewRecorder()

	rd.Redact(next).ServeHTTP(rec, req)

	// Body should pass through unchanged (aside from JSON re-serialization)
	if strings.Contains(string(capturedBody), "[EMAIL_") ||
		strings.Contains(string(capturedBody), "[PHONE_") {
		t.Errorf("clean prompt was incorrectly redacted: %s", capturedBody)
	}
}

func TestRedactMiddleware_RedactionMapInContext(t *testing.T) {
	rd := newTestRedactor()

	var mapInCtx *RedactionMap
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mapInCtx = GetRedactionMap(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	req := buildChatRequest(t, "My email is test@example.com")
	rec := httptest.NewRecorder()

	rd.Redact(next).ServeHTTP(rec, req)

	if mapInCtx == nil {
		t.Fatal("RedactionMap was not injected into context")
	}
	if !mapInCtx.HasRedactions() {
		t.Error("HasRedactions() returned false, expected true")
	}
}

func TestRedactMiddleware_FailOpen_OnBodyReadError(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rd := NewRedactor(logger, Config{Enabled: true, FailClosed: false})

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	// Simulate a request with a nil body after close
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Body = nil
	rec := httptest.NewRecorder()

	rd.Redact(next).ServeHTTP(rec, req)

	// Fail open: handler must still be called
	if !called {
		t.Error("fail-open: next handler was not called when body was nil")
	}
}

func TestRedactMiddleware_Disabled(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rd := NewRedactor(logger, Config{Enabled: false})

	var capturedBody []byte
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})

	req := buildChatRequest(t, "My email is alice@example.com")
	rec := httptest.NewRecorder()

	rd.Redact(next).ServeHTTP(rec, req)

	// When disabled, email should NOT be redacted
	if !strings.Contains(string(capturedBody), "alice@example.com") {
		t.Error("disabled redactor should pass email through unchanged")
	}
}

func TestRedactMiddleware_DisabledPatterns(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rd := NewRedactor(logger, Config{
		Enabled:          true,
		DisabledPatterns: []string{"email"},
	})
	rmap := newRedactionMap()

	result := rd.redactText("Contact alice@example.com", rmap)

	// email pattern is disabled — should NOT be redacted
	if strings.Contains(result, "[EMAIL_") {
		t.Errorf("disabled pattern 'email' should not redact, got: %q", result)
	}
}

func TestRedactMiddleware_StreamingPassthrough(t *testing.T) {
	rd := newTestRedactor()

	// Handler that simulates a streaming response
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("ResponseWriter does not implement Flusher")
		}
		_, _ = w.Write([]byte("data: chunk1\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("data: [EMAIL_1] mentioned\n\n"))
		flusher.Flush()
	})

	req := buildChatRequest(t, "Email alice@example.com")
	rec := httptest.NewRecorder()

	rd.Redact(next).ServeHTTP(rec, req)

	// For streaming: X-Gateway-Redacted header should be set
	if rec.Header().Get("X-Gateway-Redacted") == "" {
		t.Error("streaming response with PII should have X-Gateway-Redacted header")
	}
	if rec.Header().Get("X-Gateway-Redaction-Map") == "" {
		t.Error("streaming response with PII should have X-Gateway-Redaction-Map header")
	}

}
