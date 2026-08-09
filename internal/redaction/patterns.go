// File: internal/redaction/patterns.go
// WHY: Separating patterns from logic lets teams add/remove patterns without
// touching the redaction engine. Each pattern is independently testable.

package redaction

import "regexp"

// PiiPattern defines a named regex rule for detecting a single category of PII.
type PiiPattern struct {
	// Name is a machine-readable identifier used in config (e.g. REDACTION_DISABLED_PATTERNS=ipv4).
	Name string
	// Tag is the placeholder prefix emitted in redacted text (e.g. "EMAIL" → "[EMAIL_1]").
	Tag string
	// Regex is the compiled pattern. Use MustCompile — patterns are static; a bad pattern
	// is a code bug, not a runtime error.
	Regex *regexp.Regexp
	// Validator is an optional function (e.g. Luhn check) that must return true for the match to be redacted.
	Validator func(string) bool
}

// luhnCheck validates a credit card number using the Luhn algorithm.
func luhnCheck(s string) bool {
	var digits []int
	for _, r := range s {
		if r >= '0' && r <= '9' {
			digits = append(digits, int(r-'0'))
		}
	}
	if len(digits) < 13 || len(digits) > 19 {
		return false
	}
	sum := 0
	for i := len(digits) - 1; i >= 0; i-- {
		n := digits[i]
		if (len(digits)-1-i)%2 == 1 {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
	}
	return sum%10 == 0
}

// DefaultPatterns is the Phase 01 set of structural PII patterns.
// WHY regex over NER:
//   - Structural PII (email, SSN, credit card) has unambiguous syntax → regex is 100% deterministic
//   - Regex runs in microseconds; NER inference takes 50–200ms per request
//   - No external infrastructure: zero new dependencies, zero new failure modes
//   - NER is better for unstructured PII (person names, addresses) — that is Phase 2
//
// Ordering matters: run URL_CREDS before EMAIL so we don't redact the email inside
// a connection string independently and leave the password intact.
var DefaultPatterns = []PiiPattern{
	{
		// Passwords embedded in connection strings: postgres://user:pass@host/db
		// Must run BEFORE email so the user portion is captured as part of URL_CREDS.
		Name: "url_credentials",
		Tag:  "URL_CREDS",
		Regex: regexp.MustCompile(
			`[a-zA-Z][a-zA-Z0-9+\-.]+://[^:@\s"']*:[^@\s"']+@[^\s"']+`,
		),
	},
	{
		// OpenAI sk-, Stripe pk_live_/sk_live_, GitHub ghp_, GitLab glpat-, Slack xoxb-/xoxp-
		// Match common service token prefixes followed by ≥16 alphanumeric chars.
		Name: "api_key",
		Tag:  "API_KEY",
		Regex: regexp.MustCompile(
			`(?:sk-|pk_live_|pk_test_|sk_live_|sk_test_|rk_live_|rk_test_|` +
				`ghp_|gho_|ghs_|ghr_|glpat-|xoxb-|xoxp-)[A-Za-z0-9_\-]{16,}`,
		),
	},
	{
		// Standard email addresses. Intentionally simple — we prefer false positives
		// over false negatives for security.
		Name:  "email",
		Tag:   "EMAIL",
		Regex: regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`),
	},
	{
		// Credit / debit cards: 16 digits optionally separated by spaces or dashes.
		// Uses Luhn validation to avoid redacting random 16-digit numbers (like order IDs).
		Name: "credit_card",
		Tag:  "CREDIT_CARD",
		Regex: regexp.MustCompile(
			`\b[0-9]{4}[\s\-]?[0-9]{4}[\s\-]?[0-9]{4}[\s\-]?[0-9]{4}\b`,
		),
		Validator: luhnCheck,
	},
	{
		// US Social Security Numbers: NNN-NN-NNNN with optional separators.
		// The \b word boundaries prevent matching larger digit strings.
		Name:  "ssn",
		Tag:   "SSN",
		Regex: regexp.MustCompile(`\b[0-9]{3}[-\s]?[0-9]{2}[-\s]?[0-9]{4}\b`),
	},
	{
		// US phone numbers in common formats: (555) 123-4567, +1-555-123-4567, etc.
		Name: "phone_us",
		Tag:  "PHONE",
		Regex: regexp.MustCompile(
			`(?:\+1[\s\-.]?)?\(?[0-9]{3}\)?[\s\-.]?[0-9]{3}[\s\-.]?[0-9]{4}`,
		),
	},
	{
		// IPv4 addresses. NOTE: This may generate false positives for version strings
		// like "1.22.0" — operators can disable with REDACTION_DISABLED_PATTERNS=ipv4
		// if internal IP addresses are acceptable in prompts.
		Name: "ipv4",
		Tag:  "IP_ADDRESS",
		Regex: regexp.MustCompile(
			`\b(?:(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\.){3}` +
				`(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\b`,
		),
	},
}
