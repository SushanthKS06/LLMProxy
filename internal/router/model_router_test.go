// File: internal/router/model_router_test.go

package router

import (
	"context"
	"testing"

	"github.com/sushanthks/llm-gateway/internal/config"
	"github.com/stretchr/testify/assert"
)

func TestModelRouter_Route(t *testing.T) {
	cfg := &config.Config{
		Routing: config.RoutingConfig{
			DefaultModel:         "anthropic/claude-haiku-3",
			SimilarityThreshold:  0.92,
			SimpleTokenThreshold: 50,
			ComplexTokenThreshold: 300,
		},
		Cost: map[string]config.ModelCost{
			"openai/gpt-4o-mini":       {InputPer1M: 0.15, OutputPer1M: 0.60},
			"anthropic/claude-haiku-3": {InputPer1M: 0.25, OutputPer1M: 1.25},
			"anthropic/claude-sonnet-4": {InputPer1M: 3.00, OutputPer1M: 15.00},
		},
	}

	router := NewModelRouter(cfg)

	tests := []struct {
		name     string
		prompt   string
		expected string
	}{
		{
			name:     "simple prompt routes to gpt-4o-mini",
			prompt:   "What is 2+2?",
			expected: "openai/gpt-4o-mini",
		},
		{
			name:     "medium prompt routes to claude-haiku-3",
			prompt:   "Explain what a REST API is in simple terms.",
			expected: "anthropic/claude-haiku-3",
		},
		{
			name:     "complex prompt routes to claude-sonnet-4",
			prompt:   "Compare and contrast the pros and cons of microservices vs monolithic architecture. Analyze the performance implications and scalability characteristics.",
			expected: "anthropic/claude-sonnet-4",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &ProxyRequest{
				Model:      "",
				PromptText: tt.prompt,
			}
			decision := router.Route(context.Background(), req)
			assert.Equal(t, tt.expected, decision.Model)
		})
	}
}

func TestModelRouter_ModelOverride(t *testing.T) {
	cfg := &config.Config{
		Routing: config.RoutingConfig{
			DefaultModel: "anthropic/claude-haiku-3",
		},
		Cost: map[string]config.ModelCost{
			"openai/gpt-4o": {InputPer1M: 2.50, OutputPer1M: 10.00},
		},
	}

	router := NewModelRouter(cfg)

	req := &ProxyRequest{
		Model:      "openai/gpt-4o",
		PromptText: "What is 2+2?", // Would be simple, but override should win
	}
	decision := router.Route(context.Background(), req)

	assert.Equal(t, "openai/gpt-4o", decision.Model)
	assert.Equal(t, "model_override", decision.Reason)
}

func TestModelRouter_EstimateCost(t *testing.T) {
	cfg := &config.Config{
		Cost: map[string]config.ModelCost{
			"openai/gpt-4o-mini": {InputPer1M: 0.15, OutputPer1M: 0.60},
		},
	}

	router := NewModelRouter(cfg)

	// 1M input tokens * $0.15/1M + 1M output tokens * $0.60/1M = $0.75
	cost := router.EstimateCost("openai/gpt-4o-mini", 1_000_000, 1_000_000)
	assert.Equal(t, 0.75, cost)
}

func TestGetProvider(t *testing.T) {
	tests := []struct {
		model     string
		expected  string
	}{
		{"openai/gpt-4o", "openai"},
		{"openai/gpt-4o-mini", "openai"},
		{"anthropic/claude-haiku-3", "anthropic"},
		{"anthropic/claude-sonnet-4", "anthropic"},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			result := getProvider(tt.model)
			assert.Equal(t, tt.expected, result)
		})
	}
}
