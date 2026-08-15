// File: internal/router/model_router.go
// WHY: Routes prompts to the cheapest capable model based on complexity.
// Respects X-Gateway-Model header override for explicit model selection.

package router

import (
	"context"
	"fmt"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/sushanthks/llm-gateway/internal/config"
)

var (
	routeDecisions = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_route_decisions_total",
		Help: "Total number of routing decisions",
	}, []string{"model", "complexity", "reason"})
)

// RouteDecision represents the routing decision for a request.
type RouteDecision struct {
	Model            string
	Provider         string
	EstimatedCostUSD float64
	Reason           string
}

// ProxyRequest represents an incoming proxy request.
type ProxyRequest struct {
	Model      string
	Messages   []Message
	TeamID     string
	FeatureTag string
	PromptText string
}

// Message represents a chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ModelRouter handles model routing decisions.
type ModelRouter struct {
	config     *config.RoutingConfig
	costConfig map[string]config.ModelCost
	classifier *ComplexityClassifier
}

// NewModelRouter creates a new ModelRouter instance.
func NewModelRouter(cfg *config.Config) *ModelRouter {
	return &ModelRouter{
		config:     &cfg.Routing,
		costConfig: cfg.Cost,
		classifier: NewComplexityClassifier(cfg.Routing.SimpleTokenThreshold, cfg.Routing.ComplexTokenThreshold),
	}
}

// Route determines the model to use for a request.
func (mr *ModelRouter) Route(ctx context.Context, req *ProxyRequest) RouteDecision {
	// Check for model override header
	if req.Model != "" {
		decision := RouteDecision{
			Model:    req.Model,
			Provider: getProvider(req.Model),
			Reason:   "model_override",
		}
		routeDecisions.WithLabelValues(decision.Model, "override", decision.Reason).Inc()
		return decision
	}

	// Classify complexity
	complexity := mr.classifier.Classify(req.PromptText)

	// Route based on complexity
	var model string
	var reason string

	switch complexity {
	case ComplexitySimple:
		model = "groq/gpt-oss-20b"
		reason = "simple_prompt"
	case ComplexityMedium:
		model = "groq/llama-3.3-70b-versatile"
		reason = "medium_prompt"
	case ComplexityComplex:
		model = "groq/gpt-oss-120b"
		reason = "complex_prompt"
	default:
		model = mr.config.DefaultModel
		reason = "default_model"
	}

	decision := RouteDecision{
		Model:    model,
		Provider: getProvider(model),
		Reason:   reason,
	}

	routeDecisions.WithLabelValues(decision.Model, string(complexity), decision.Reason).Inc()

	return decision
}

// EstimateCost estimates the cost for a request based on model and token counts.
func (mr *ModelRouter) EstimateCost(model string, inputTokens, outputTokens int) float64 {
	cost, ok := mr.costConfig[model]
	if !ok {
		// Default to claude-haiku-3 pricing if model not found
		cost = mr.costConfig["anthropic/claude-haiku-3"]
	}

	inputCost := float64(inputTokens) / 1_000_000 * cost.InputPer1M
	outputCost := float64(outputTokens) / 1_000_000 * cost.OutputPer1M

	return inputCost + outputCost
}

// GetModelCost returns the cost configuration for a model.
func (mr *ModelRouter) GetModelCost(model string) (config.ModelCost, bool) {
	cost, ok := mr.costConfig[model]
	return cost, ok
}

// GetProvider returns the provider name for a model identifier.
// Models follow the format "provider/model-name" (e.g. "openai/gpt-4o").
// Exported so other packages (proxy) can reuse it without duplicating the logic.
func GetProvider(model string) string {
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

// getProvider is the package-private alias kept for internal use.
func getProvider(model string) string { return GetProvider(model) }

// GetComplexity returns the complexity classification for a prompt.
func (mr *ModelRouter) GetComplexity(prompt string) ComplexityLevel {
	return mr.classifier.Classify(prompt)
}

// ValidateModel checks if a model is valid.
func (mr *ModelRouter) ValidateModel(model string) error {
	_, ok := mr.costConfig[model]
	if !ok {
		return fmt.Errorf("unknown model: %s", model)
	}
	return nil
}
