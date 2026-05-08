// File: internal/router/model_router.go
// WHY: Routes prompts to the cheapest capable model based on complexity.
// Respects X-Gateway-Model header override for explicit model selection.

package router

import (
	"context"
	"fmt"

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
	Model        string
	Messages     []Message
	TeamID       string
	FeatureTag   string
	PromptText   string
}

// Message represents a chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ModelRouter handles model routing decisions.
type ModelRouter struct {
	config      *config.RoutingConfig
	costConfig  map[string]config.ModelCost
	classifier  *ComplexityClassifier
}

// NewModelRouter creates a new ModelRouter instance.
func NewModelRouter(cfg *config.Config) *ModelRouter {
	return &ModelRouter{
		config:      &cfg.Routing,
		costConfig:  cfg.Cost,
		classifier:  NewComplexityClassifier(cfg.Routing.SimpleTokenThreshold, cfg.Routing.ComplexTokenThreshold),
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
		model = "openai/gpt-4o-mini"
		reason = "simple_prompt"
	case ComplexityMedium:
		model = "anthropic/claude-haiku-3"
		reason = "medium_prompt"
	case ComplexityComplex:
		model = "anthropic/claude-sonnet-4"
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

// getProvider returns the provider for a model.
func getProvider(model string) string {
	switch {
	case contains(model, "openai"):
		return "openai"
	case contains(model, "anthropic"):
		return "anthropic"
	default:
		return "unknown"
	}
}

// contains checks if s contains substr.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

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
