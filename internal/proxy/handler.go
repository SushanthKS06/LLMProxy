// File: internal/proxy/handler.go
// WHY: Core HTTP handler for the gateway. Handles the full request lifecycle:
// cache lookup → routing → provider forward → streaming response → async logging.
//
// FIX (ISSUE-02): http.MaxBytesReader applied before JSON decode — prevents
//   malicious or accidental multi-GB request bodies from exhausting memory.
// FIX (BUG-07): proxyRequests metric now reflects the actual HTTP status code
//   returned to the client instead of a hardcoded "200" on all success paths.
// FIX (MINOR-01): Removed unused estimateTokens() function (dead code).

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/sushanthks/llm-gateway/internal/cache"
	"github.com/sushanthks/llm-gateway/internal/config"
	"github.com/sushanthks/llm-gateway/internal/router"
	"github.com/sushanthks/llm-gateway/internal/tracker"
)

// maxRequestBodyBytes is the upper bound on request body size (10 MiB).
// FIX (ISSUE-02): prevents malicious/accidental huge bodies from exhausting memory.
const maxRequestBodyBytes = 10 * 1024 * 1024

var (
	proxyRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_proxy_requests_total",
		Help: "Total number of proxy requests",
	}, []string{"method", "status"})

	proxyErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_proxy_errors_total",
		Help: "Total number of proxy errors",
	}, []string{"error_type"})
)

// GatewayHandler handles incoming proxy requests.
type GatewayHandler struct {
	cache   *cache.SemanticCache
	router  *router.ModelRouter
	tracker *tracker.CostTracker
	config  *config.Config
	client  *http.Client
}

// NewGatewayHandler creates a new GatewayHandler.
func NewGatewayHandler(
	semanticCache *cache.SemanticCache,
	modelRouter *router.ModelRouter,
	costTracker *tracker.CostTracker,
	cfg *config.Config,
) *GatewayHandler {
	return &GatewayHandler{
		cache:   semanticCache,
		router:  modelRouter,
		tracker: costTracker,
		config:  cfg,
		client: &http.Client{
			Timeout: 120 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// ServeHTTP handles the incoming request.
// Full request lifecycle: validate → extract headers → cache lookup → route → forward → respond.
func (h *GatewayHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	ctx := r.Context()

	// Validate method
	if r.Method != http.MethodPost {
		h.writeError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// FIX (ISSUE-02): cap request body to 10 MiB before reading.
	// Without this, a client sending a multi-GB body exhausts gateway memory.
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)

	// Extract gateway control headers
	teamID := r.Header.Get("X-Gateway-Team")
	if teamID == "" {
		teamID = "default"
	}

	featureTag := r.Header.Get("X-Gateway-Feature")
	if featureTag == "" {
		featureTag = "untagged"
	}

	// X-Gateway-Model lets callers override automatic model routing
	modelOverride := r.Header.Get("X-Gateway-Model")

	// Parse request body
	var reqBody chatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
		h.writeError(w, r, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}

	// Extract the user's prompt text (first "user" role message)
	promptText := extractPromptText(reqBody.Messages)
	if promptText == "" {
		h.writeError(w, r, http.StatusBadRequest, "no prompt found in messages")
		return
	}

	// ── Layer 1 + Layer 2 cache lookup ──────────────────────────────────────
	cacheResult, err := h.cache.Lookup(ctx, promptText, teamID)
	if err != nil {
		proxyErrors.WithLabelValues("cache_lookup").Inc()
		// Non-fatal: continue without cache on error
	}

	if cacheResult != nil {
		// Cache hit (exact hash or semantic similarity >= threshold)
		h.handleCacheHit(w, r, cacheResult, start, teamID, featureTag, promptText)
		proxyRequests.WithLabelValues(r.Method, "200").Inc()
		return
	}

	// ── Cache miss: classify complexity and route to cheapest capable model ──
	routeDecision := h.router.Route(ctx, &router.ProxyRequest{
		Model:      modelOverride,
		PromptText: promptText,
		TeamID:     teamID,
		FeatureTag: featureTag,
	})

	// ── Forward to LLM provider ──────────────────────────────────────────────
	response, providerLatency, err := h.forwardToProvider(ctx, r, &reqBody, routeDecision.Model, routeDecision.Provider)
	if err != nil {
		proxyErrors.WithLabelValues("provider_forward").Inc()
		h.writeError(w, r, http.StatusBadGateway, fmt.Sprintf("provider error: %v", err))
		return
	}
	defer response.Body.Close()

	// ── Respond to client ────────────────────────────────────────────────────
	// FIX (BUG-07): capture the actual status code written to the client so
	// the proxyRequests metric is accurate for non-200 provider responses.
	if reqBody.Stream {
		h.handleStreaming(w, r, response, routeDecision.Model, teamID, featureTag, promptText, providerLatency, start)
		proxyRequests.WithLabelValues(r.Method, "200").Inc()
	} else {
		statusCode := h.handleNonStreaming(w, r, response, routeDecision.Model, teamID, featureTag, providerLatency, start, promptText)
		proxyRequests.WithLabelValues(r.Method, fmt.Sprintf("%d", statusCode)).Inc()
	}
}

// handleCacheHit writes a cached LLM response directly to the client.
// promptText is used for complexity classification (not the response text).
func (h *GatewayHandler) handleCacheHit(w http.ResponseWriter, r *http.Request, result *cache.CacheResult, start time.Time, teamID, featureTag, promptText string) {
	latency := time.Since(start).Milliseconds()

	w.Header().Set("Content-Type", "application/json")
	if result.Similarity >= 1.0 {
		w.Header().Set("X-Gateway-Cache", "redis_hit")
	} else {
		w.Header().Set("X-Gateway-Cache", "semantic_hit")
	}
	w.Header().Set("X-Gateway-Model", result.ModelUsed)
	w.Header().Set("X-Gateway-Cost-USD", fmt.Sprintf("%.8f", result.CostUSD))
	w.Header().Set("X-Gateway-Latency-MS", fmt.Sprintf("%d", latency))

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(result.ResponseJSON))

	// Async: record cache hit usage (tokens=0, cost from cached entry)
	h.tracker.RecordUsage(&tracker.UsageEvent{
		TeamID:       teamID,
		FeatureTag:   featureTag,
		ModelUsed:    result.ModelUsed,
		Provider:     getProviderFromModel(result.ModelUsed),
		InputTokens:  0, // Not re-charged for cache hits
		OutputTokens: 0,
		CostUSD:      result.CostUSD,
		LatencyMS:    int(latency),
		CacheHit:     true,
		Complexity:   string(h.router.GetComplexity(promptText)), // use original prompt, not response JSON
	})
}

// handleStreaming forwards a streaming SSE response to the client chunk-by-chunk.
// Cost is reported as an HTTP trailer (sent after body) so we can compute
// the actual token count from the full streamed response before setting it.
func (h *GatewayHandler) handleStreaming(w http.ResponseWriter, r *http.Request, response *http.Response, model, teamID, featureTag, promptText string, providerLatency int, start time.Time) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Gateway-Cache", "miss")
	w.Header().Set("X-Gateway-Model", model)
	w.Header().Set("X-Gateway-Latency-MS", fmt.Sprintf("%d", providerLatency))
	// Declare trailer key BEFORE first Flush so the client knows to expect it
	w.Header().Set("Trailer", "X-Gateway-Cost-USD")

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.writeError(w, r, http.StatusInternalServerError, "streaming not supported")
		return
	}

	buf := make([]byte, 4096)
	var fullResponse strings.Builder

	for {
		n, err := response.Body.Read(buf)
		if n > 0 {
			chunk := string(buf[:n])
			fullResponse.WriteString(chunk)
			fmt.Fprint(w, chunk)
			flusher.Flush()
		}
		if err != nil {
			break
		}
	}

	// Now that streaming is done, compute actual cost from the full response
	inputTokens := estimateTokensFromPrompt(promptText)
	outputTokens := estimateTokensFromResponse(fullResponse.String())
	cost := h.router.EstimateCost(model, inputTokens, outputTokens)
	latency := time.Since(start).Milliseconds()

	// Set cost as HTTP/1.1 chunked trailer (sent after last chunk)
	// Clients that don't support trailers simply ignore this header.
	w.Header().Set(http.TrailerPrefix+"X-Gateway-Cost-USD", fmt.Sprintf("%.8f", cost))

	h.tracker.RecordUsage(&tracker.UsageEvent{
		TeamID:       teamID,
		FeatureTag:   featureTag,
		ModelUsed:    model,
		Provider:     getProviderFromModel(model),
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		CostUSD:      cost,
		LatencyMS:    int(latency),
		CacheHit:     false,
		Complexity:   string(h.router.GetComplexity(promptText)),
	})
}

// handleNonStreaming reads the full provider response, sends it to the client,
// then asynchronously stores it in the semantic cache.
// Returns the HTTP status code written to the client (used for metrics).
func (h *GatewayHandler) handleNonStreaming(w http.ResponseWriter, r *http.Request, response *http.Response, model, teamID, featureTag string, providerLatency int, start time.Time, promptText string) int {
	body, err := io.ReadAll(response.Body)
	if err != nil {
		h.writeError(w, r, http.StatusBadGateway, fmt.Sprintf("failed to read response: %v", err))
		return http.StatusBadGateway
	}

	latency := time.Since(start).Milliseconds()

	inputTokens := estimateTokensFromPrompt(promptText)
	outputTokens := estimateTokensFromResponse(string(body))
	cost := h.router.EstimateCost(model, inputTokens, outputTokens)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Gateway-Cache", "miss")
	w.Header().Set("X-Gateway-Model", model)
	w.Header().Set("X-Gateway-Latency-MS", fmt.Sprintf("%d", latency))
	w.Header().Set("X-Gateway-Cost-USD", fmt.Sprintf("%.8f", cost))

	// FIX (BUG-07): use provider's actual status code, not hardcoded 200.
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(body)

	// Async: store response in semantic cache for future similar prompts.
	// Only cache successful provider responses (2xx).
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		h.storeInCache(promptText, string(body), model, teamID, inputTokens, outputTokens, cost)
	}

	h.tracker.RecordUsage(&tracker.UsageEvent{
		TeamID:       teamID,
		FeatureTag:   featureTag,
		ModelUsed:    model,
		Provider:     getProviderFromModel(model),
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		CostUSD:      cost,
		LatencyMS:    int(latency),
		CacheHit:     false,
		Complexity:   string(h.router.GetComplexity(promptText)),
	})

	// FIX (BUG-07): return the actual status code so the caller can record
	// the correct metric label.
	return response.StatusCode
}

// forwardToProvider constructs and sends the request to the appropriate LLM provider.
// It handles provider-specific URL, authentication headers, and request format conversion.
func (h *GatewayHandler) forwardToProvider(ctx context.Context, r *http.Request, reqBody *chatCompletionRequest, model, provider string) (*http.Response, int, error) {
	var url string
	headers := make(http.Header)

	switch provider {
	case "openai":
		url = fmt.Sprintf("%s/chat/completions", h.config.Providers.OpenAIBaseURL)
		headers.Set("Authorization", "Bearer "+h.config.Providers.OpenAIAPIKey)
	case "anthropic":
		url = fmt.Sprintf("%s/v1/messages", h.config.Providers.AnthropicBaseURL)
		headers.Set("x-api-key", h.config.Providers.AnthropicAPIKey)
		headers.Set("anthropic-version", "2023-06-01")
	case "groq":
		url = fmt.Sprintf("%s/chat/completions", h.config.Providers.GroqBaseURL)
		headers.Set("Authorization", "Bearer "+h.config.Providers.GroqAPIKey)
	default:
		return nil, 0, fmt.Errorf("unknown provider: %s", provider)
	}

	headers.Set("Content-Type", "application/json")

	providerBody := convertToProviderFormat(reqBody, provider, model)
	bodyBytes, err := json.Marshal(providerBody)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to marshal request body: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, 0, err
	}

	httpReq.Header = headers

	var resp *http.Response

	maxRetries := 3
	backoff := 100 * time.Millisecond

	start := time.Now()
	for i := 0; i < maxRetries; i++ {
		// Clone body reader for retries
		httpReq.Body = io.NopCloser(bytes.NewReader(bodyBytes))

		resp, err = h.client.Do(httpReq)
		if err == nil && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			break
		}

		// If it's the last attempt, don't sleep
		if i == maxRetries-1 {
			break
		}

		// Drain and close response body before retrying
		if resp != nil && resp.Body != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}

		time.Sleep(backoff)
		backoff *= 2
	}
	latency := int(time.Since(start).Milliseconds())

	return resp, latency, err
}

// storeInCache persists the LLM response to the semantic cache asynchronously.
func (h *GatewayHandler) storeInCache(prompt, response, model, teamID string, inputTokens, outputTokens int, cost float64) {
	// StoreResponse is already async (launches goroutine internally)
	h.cache.StoreResponse(context.Background(), prompt, response, model, inputTokens, outputTokens, cost, teamID)
}

// writeError writes a structured JSON error response.
func (h *GatewayHandler) writeError(w http.ResponseWriter, r *http.Request, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
	proxyRequests.WithLabelValues(r.Method, fmt.Sprintf("%d", status)).Inc()
}

// ─── Request / Response Types ────────────────────────────────────────────────

// chatCompletionRequest is the OpenAI-compatible request body accepted by the gateway.
type chatCompletionRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Stream      bool      `json:"stream,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Temperature float64   `json:"temperature,omitempty"`
}

// Message is a single chat message in role/content format.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// anthropicRequest matches the Anthropic Messages API request body.
// WHY: Anthropic's schema differs from OpenAI's in three key ways:
//  1. max_tokens is REQUIRED (no default)
//  2. system prompt is a top-level field, not a message
//  3. Model IDs use a different naming convention (e.g. claude-3-haiku-20240307)
type anthropicRequest struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	Messages  []Message `json:"messages"`
	System    string    `json:"system,omitempty"`
	Stream    bool      `json:"stream,omitempty"`
}

// ─── Helper Functions ─────────────────────────────────────────────────────────

// extractPromptText returns the content of the first user message.
func extractPromptText(messages []Message) string {
	for _, msg := range messages {
		if msg.Role == "user" {
			return msg.Content
		}
	}
	return ""
}

// extractPromptTextFromJSON extracts the assistant reply content from an OpenAI response JSON.
// Returns "" if the JSON cannot be parsed or the expected structure is absent.
func extractPromptTextFromJSON(resp string) string {
	var respMap map[string]interface{}
	if err := json.Unmarshal([]byte(resp), &respMap); err != nil {
		return ""
	}
	choices, ok := respMap["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return ""
	}
	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		return ""
	}
	message, ok := choice["message"].(map[string]interface{})
	if !ok {
		return ""
	}
	content, ok := message["content"].(string)
	if !ok {
		return ""
	}
	return content
}

// getProviderFromModel returns the provider name from an internal model identifier.
// Internal format: "provider/model-name" (e.g. "openai/gpt-4o-mini").
func getProviderFromModel(model string) string {
	switch {
	case strings.HasPrefix(model, "openai/"):
		return "openai"
	case strings.HasPrefix(model, "anthropic/"):
		return "anthropic"
	case strings.HasPrefix(model, "groq/"):
		return "groq"
	default:
		return "unknown"
	}
}

// estimateTokensFromPrompt estimates tokens in a user prompt using a character heuristic.
// WHY: A standard rule of thumb is 1 token ~= 4 characters in English text.
// This is much more accurate for cost accounting than word counts.
func estimateTokensFromPrompt(prompt string) int {
	return len(prompt) / 4
}

// estimateTokensFromResponse estimates tokens in a JSON response string.
func estimateTokensFromResponse(resp string) int {
	return len(resp) / 4
}

// convertToProviderFormat converts the internal request body to the provider's
// expected wire format. OpenAI and Anthropic have different API schemas.
func convertToProviderFormat(req *chatCompletionRequest, provider, model string) interface{} {
	switch provider {
	case "anthropic":
		return convertToAnthropicFormat(req, model)
	case "groq":
		req.Model = mapGroqModelName(model)
		return req
	default: // openai
		req.Model = mapOpenAIModelName(model)
		return req
	}
}

// mapGroqModelName converts internal model identifiers to Groq API model IDs.
func mapGroqModelName(model string) string {
	mapping := map[string]string{
		"groq/gpt-oss-20b":             "openai/gpt-oss-20b",
		"groq/llama-3.3-70b-versatile": "llama-3.3-70b-versatile",
		"groq/gpt-oss-120b":            "openai/gpt-oss-120b",
	}
	if mapped, ok := mapping[model]; ok {
		return mapped
	}
	// Fallback: strip the "groq/" prefix
	return strings.TrimPrefix(model, "groq/")
}

// convertToAnthropicFormat builds the Anthropic Messages API body from the
// OpenAI-compatible request. Key differences handled:
// - max_tokens is required; defaults to 1024 when caller didn't specify
// - system messages extracted to top-level "system" field
// - model ID mapped to the Anthropic API model identifier
func convertToAnthropicFormat(req *chatCompletionRequest, model string) *anthropicRequest {
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = 1024
	}

	var system string
	messages := make([]Message, 0, len(req.Messages))
	for _, msg := range req.Messages {
		if msg.Role == "system" {
			system = msg.Content // Anthropic takes system as a top-level field
		} else {
			messages = append(messages, msg)
		}
	}

	return &anthropicRequest{
		Model:     mapAnthropicModelName(model),
		MaxTokens: maxTokens,
		Messages:  messages,
		System:    system,
		Stream:    req.Stream,
	}
}

// mapAnthropicModelName converts internal model identifiers to Anthropic API model IDs.
func mapAnthropicModelName(model string) string {
	mapping := map[string]string{
		"anthropic/claude-haiku-3":  "claude-3-haiku-20240307",
		"anthropic/claude-sonnet-4": "claude-sonnet-4-5",
	}
	if mapped, ok := mapping[model]; ok {
		return mapped
	}
	// Fallback: strip the "anthropic/" prefix
	return strings.TrimPrefix(model, "anthropic/")
}

// mapOpenAIModelName converts internal model identifiers to OpenAI API model IDs.
func mapOpenAIModelName(model string) string {
	mapping := map[string]string{
		"openai/gpt-4o-mini": "gpt-4o-mini",
		"openai/gpt-4o":      "gpt-4o",
	}
	if mapped, ok := mapping[model]; ok {
		return mapped
	}
	// Fallback: strip the "openai/" prefix
	return strings.TrimPrefix(model, "openai/")
}
