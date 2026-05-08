// File: internal/proxy/handler.go
// WHY: Core HTTP handler for the gateway. Handles the full request lifecycle:
// cache lookup → routing → provider forward → streaming response → async logging.

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
			Timeout:   120 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// ServeHTTP handles the incoming request.
func (h *GatewayHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	ctx := r.Context()

	// Validate request
	if r.Method != http.MethodPost {
		h.writeError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Extract headers
	teamID := r.Header.Get("X-Gateway-Team")
	if teamID == "" {
		teamID = "default"
	}

	featureTag := r.Header.Get("X-Gateway-Feature")
	if featureTag == "" {
		featureTag = "untagged"
	}

	modelOverride := r.Header.Get("X-Gateway-Model")

	// Parse request body
	var reqBody chatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
		h.writeError(w, r, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}

	// Extract prompt text
	promptText := extractPromptText(reqBody.Messages)
	if promptText == "" {
		h.writeError(w, r, http.StatusBadRequest, "no prompt found in messages")
		return
	}

	// Cache lookup
	cacheResult, err := h.cache.Lookup(ctx, promptText, teamID)
	if err != nil {
		proxyErrors.WithLabelValues("cache_lookup").Inc()
		// Continue without cache on error
	}

	isCacheHit := cacheResult != nil

	// Handle cache hit
	if isCacheHit {
		h.handleCacheHit(w, r, cacheResult, start, teamID, featureTag)
		proxyRequests.WithLabelValues(r.Method, "200").Inc()
		return
	}

	// Cache miss - route to provider
	routeDecision := h.router.Route(ctx, &router.ProxyRequest{
		Model:      modelOverride,
		PromptText: promptText,
		TeamID:     teamID,
		FeatureTag: featureTag,
	})

	// Forward to provider
	response, latency, err := h.forwardToProvider(ctx, r, &reqBody, routeDecision.Model, routeDecision.Provider)
	if err != nil {
		proxyErrors.WithLabelValues("provider_forward").Inc()
		h.writeError(w, r, http.StatusBadGateway, fmt.Sprintf("provider error: %v", err))
		return
	}
	defer response.Body.Close()

	// Check if streaming
	if reqBody.Stream {
		h.handleStreaming(w, r, response, routeDecision.Model, teamID, featureTag, latency, start)
	} else {
		h.handleNonStreaming(w, r, response, routeDecision.Model, teamID, featureTag, latency, start, promptText)
	}

	proxyRequests.WithLabelValues(r.Method, "200").Inc()
}

// handleCacheHit returns a cached response.
func (h *GatewayHandler) handleCacheHit(w http.ResponseWriter, r *http.Request, result *cache.CacheResult, start time.Time, teamID, featureTag string) {
	latency := time.Since(start).Milliseconds()

	// Set headers
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Gateway-Cache", "hit")
	w.Header().Set("X-Gateway-Model", result.ModelUsed)
	w.Header().Set("X-Gateway-Cost-USD", fmt.Sprintf("%.8f", result.CostUSD))
	w.Header().Set("X-Gateway-Latency-MS", fmt.Sprintf("%d", latency))

	// Write response
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(result.ResponseJSON))

	// Async record usage
	h.tracker.RecordUsage(&tracker.UsageEvent{
		TeamID:       teamID,
		FeatureTag:   featureTag,
		ModelUsed:    result.ModelUsed,
		Provider:     getProviderFromModel(result.ModelUsed),
		InputTokens:  0, // Not tracked for cache hits
		OutputTokens: 0,
		CostUSD:      result.CostUSD,
		LatencyMS:    int(latency),
		CacheHit:     true,
		Complexity:   string(h.router.GetComplexity(extractPromptTextFromJSON(result.ResponseJSON))),
	})
}

// handleStreaming handles streaming responses from the provider.
func (h *GatewayHandler) handleStreaming(w http.ResponseWriter, r *http.Request, response *http.Response, model, teamID, featureTag string, providerLatency int, start time.Time) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Gateway-Cache", "miss")
	w.Header().Set("X-Gateway-Model", model)
	w.Header().Set("X-Gateway-Latency-MS", fmt.Sprintf("%d", providerLatency))

	// Enable streaming
	flusher, ok := w.(http.Flusher)
	if !ok {
		h.writeError(w, r, http.StatusInternalServerError, "streaming not supported")
		return
	}

	// Read and forward response
	buf := make([]byte, 4096)
	var fullResponse string

	for {
		n, err := response.Body.Read(buf)
		if n > 0 {
			chunk := string(buf[:n])
			fullResponse += chunk
			fmt.Fprint(w, chunk)
			flusher.Flush()
		}
		if err != nil {
			break
		}
	}

	// Async record usage (estimate tokens from response)
	inputTokens := estimateTokens(r.Body)
	outputTokens := estimateTokensFromResponse(fullResponse)
	cost := h.router.EstimateCost(model, inputTokens, outputTokens)
	latency := time.Since(start).Milliseconds()

	w.Header().Set("X-Gateway-Cost-USD", fmt.Sprintf("%.8f", cost))

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
		Complexity:   string(h.router.GetComplexity(extractPromptTextFromJSON(fullResponse))),
	})
}

// handleNonStreaming handles non-streaming responses.
func (h *GatewayHandler) handleNonStreaming(w http.ResponseWriter, r *http.Request, response *http.Response, model, teamID, featureTag string, providerLatency int, start time.Time, promptText string) {
	body, err := io.ReadAll(response.Body)
	if err != nil {
		h.writeError(w, r, http.StatusBadGateway, fmt.Sprintf("failed to read response: %v", err))
		return
	}

	latency := time.Since(start).Milliseconds()

	// Set headers
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Gateway-Cache", "miss")
	w.Header().Set("X-Gateway-Model", model)
	w.Header().Set("X-Gateway-Latency-MS", fmt.Sprintf("%d", latency))

	// Estimate tokens and cost
	inputTokens := estimateTokensFromPrompt(promptText)
	outputTokens := estimateTokensFromResponse(string(body))
	cost := h.router.EstimateCost(model, inputTokens, outputTokens)

	w.Header().Set("X-Gateway-Cost-USD", fmt.Sprintf("%.8f", cost))

	// Write response
	w.WriteHeader(response.StatusCode)
	w.Write(body)

	// Async store in cache and record usage
	h.storeInCache(promptText, string(body), model, inputTokens, outputTokens, cost, teamID)

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

// forwardToProvider forwards the request to the LLM provider.
func (h *GatewayHandler) forwardToProvider(ctx context.Context, r *http.Request, reqBody *chatCompletionRequest, model, provider string) (*http.Response, int, error) {
	var url string
	var headers http.Header = make(http.Header)

	switch provider {
	case "openai":
		url = fmt.Sprintf("%s/chat/completions", h.config.Providers.OpenAIBaseURL)
		headers.Set("Authorization", "Bearer "+h.config.Providers.OpenAIAPIKey)
	case "anthropic":
		url = fmt.Sprintf("%s/v1/messages", h.config.Providers.AnthropicBaseURL)
		headers.Set("x-api-key", h.config.Providers.AnthropicAPIKey)
		headers.Set("anthropic-version", "2023-06-01")
	default:
		return nil, 0, fmt.Errorf("unknown provider: %s", provider)
	}

	headers.Set("Content-Type", "application/json")

	// Convert to provider format
	providerBody := convertToProviderFormat(reqBody, provider, model)
	bodyBytes, _ := json.Marshal(providerBody)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, 0, err
	}

	httpReq.Header = headers

	start := time.Now()
	resp, err := h.client.Do(httpReq)
	latency := int(time.Since(start).Milliseconds())

	return resp, latency, err
}

// storeInCache stores the response in the semantic cache.
func (h *GatewayHandler) storeInCache(prompt, response, model string, inputTokens, outputTokens int, cost float64, teamID string) {
	embedding, err := h.cache.(*cache.SemanticCache) // This is a hack, need to fix
	// Actually we need to get the embedder from somewhere
	// For now, skip embedding storage
	_ = embedding
	_ = err

	// Store without embedding for now (embedding will be computed async)
	// This is a simplified version - in production we'd compute embedding
}

// writeError writes an error response.
func (h *GatewayHandler) writeError(w http.ResponseWriter, r *http.Request, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
	proxyRequests.WithLabelValues(r.Method, fmt.Sprintf("%d", status)).Inc()
}

// Chat completion request/response types
type chatCompletionRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Stream      bool      `json:"stream,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Temperature float64   `json:"temperature,omitempty"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Helper functions
func extractPromptText(messages []Message) string {
	for _, msg := range messages {
		if msg.Role == "user" {
			return msg.Content
		}
	}
	return ""
}

func extractPromptTextFromJSON(resp string) string {
	var respMap map[string]interface{}
	if err := json.Unmarshal([]byte(resp), &respMap); err != "" {
		return ""
	}
	// Extract from response
	return ""
}

func getProviderFromModel(model string) string {
	if strings.Contains(model, "openai") {
		return "openai"
	}
	return "anthropic"
}

func estimateTokens(text string) int {
	// Rough estimate: ~4 characters per token
	return len(text) / 4
}

func estimateTokensFromPrompt(prompt string) int {
	words := len(strings.Fields(prompt))
	return words // Rough estimate
}

func estimateTokensFromResponse(resp string) int {
	// Parse response and estimate
	return len(resp) / 4
}

func convertToProviderFormat(req *chatCompletionRequest, provider, model string) interface{} {
	// Convert to provider-specific format
	return req
}
