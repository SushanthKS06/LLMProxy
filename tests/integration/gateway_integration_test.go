package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/joho/godotenv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sushanthks/llm-gateway/internal/cache"
	"github.com/sushanthks/llm-gateway/internal/config"
	"github.com/sushanthks/llm-gateway/internal/db"
	"github.com/sushanthks/llm-gateway/internal/proxy"
	"github.com/sushanthks/llm-gateway/internal/redaction"
	"github.com/sushanthks/llm-gateway/internal/router"
	"github.com/sushanthks/llm-gateway/internal/tracker"
)

func TestGatewayIntegration(t *testing.T) {
	_ = godotenv.Load("../../.env")
	
	pgDSN := os.Getenv("POSTGRES_DSN")
	redisAddr := os.Getenv("REDIS_ADDR")
	if pgDSN == "" || redisAddr == "" {
		t.Skip("Skipping integration test: POSTGRES_DSN and REDIS_ADDR must be set")
	}

	// 1. Setup mock LLM Provider
	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read body to check for redaction
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		bodyStr := string(body)

		// Assert redaction is applied
		// FIX (P2-1): The SSN pattern tag is "SSN" (see patterns.go), which produces
		// the placeholder "[SSN_1]", NOT "[REDACTED_US_SSN]" as the old assertion claimed.
		if strings.Contains(bodyStr, "my ssn is") {
			assert.Contains(t, bodyStr, "[SSN_1]")
			assert.NotContains(t, bodyStr, "123-45-6789")
		}

		w.Header().Set("Content-Type", "application/json")
		// Send mock response
		respJSON := `{"id":"chatcmpl-123","object":"chat.completion","created":1677652288,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"I have processed your request."},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":12,"total_tokens":21}}`
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(respJSON))
	}))
	defer mockLLM.Close()

	// 2. Setup gateway components
	os.Setenv("GROQ_BASE_URL", mockLLM.URL)
	os.Setenv("REDACTION_ENABLED", "true")
	os.Setenv("OPENAI_API_KEY", "dummy")
	os.Setenv("ANTHROPIC_API_KEY", "dummy")
	os.Setenv("GROQ_API_KEY", "dummy")

	cfg := config.Load()
	cfg.Providers.GroqBaseURL = mockLLM.URL
	
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	
	ctx := context.Background()
	pool, err := db.NewPool(ctx, &cfg.Database)
	require.NoError(t, err)
	defer pool.Close()
	
	// Clear prompt cache
	_, err = pool.Exec(ctx, "TRUNCATE TABLE prompt_cache CASCADE")
	require.NoError(t, err)

	redisClient := db.NewRedisClient(&cfg.Database)
	if err := redisClient.Ping(ctx).Err(); err != nil {
		t.Skip("Redis is not available")
	}
	defer redisClient.Close()
	redisClient.FlushDB(ctx)

	embedder := cache.NewEmbedder(cfg, redisClient)
	defer embedder.Close()

	semanticCache := cache.NewSemanticCache(pool, redisClient, embedder, &cfg.Routing)
	modelRouter := router.NewModelRouter(cfg)
	costTracker := tracker.NewCostTracker(ctx, pool)
	defer costTracker.Close()

	gatewayHandler := proxy.NewGatewayHandler(semanticCache, modelRouter, costTracker, cfg)

	// Build middleware chain
	redactorMiddleware := redaction.NewRedactor(logger, redaction.Config{
		Enabled: true,
	})

	chain := proxy.Chain(
		gatewayHandler,
		redactorMiddleware.Redact,
	)

	server := httptest.NewServer(chain)
	defer server.Close()

	// 3. Round Trip 1: Cache Miss + Redaction
	reqBody := map[string]interface{}{
		"model": "gpt-4o-mini",
		"messages": []map[string]string{
			{"role": "user", "content": "Hello, my ssn is 123-45-6789. What is the weather?"},
		},
	}
	reqBytes, _ := json.Marshal(reqBody)
	
	req1, _ := http.NewRequest("POST", server.URL+"/v1/chat/completions", bytes.NewReader(reqBytes))
	req1.Header.Set("Content-Type", "application/json")
	
	client := &http.Client{Timeout: 10 * time.Second}
	resp1, err := client.Do(req1)
	require.NoError(t, err)
	defer resp1.Body.Close()
	
	assert.Equal(t, http.StatusOK, resp1.StatusCode)
	
	// Check headers for cache miss
	assert.Equal(t, "miss", resp1.Header.Get("X-Gateway-Cache"))
	
	// The response should have the SSN restored if it appeared in the response, but our mock doesn't echo it.
	
	// Wait for cache to populate async
	time.Sleep(2 * time.Second)

	// 4. Round Trip 2: Cache Hit
	req2, _ := http.NewRequest("POST", server.URL+"/v1/chat/completions", bytes.NewReader(reqBytes))
	req2.Header.Set("Content-Type", "application/json")
	
	resp2, err := client.Do(req2)
	require.NoError(t, err)
	defer resp2.Body.Close()
	
	assert.Equal(t, http.StatusOK, resp2.StatusCode)
	
	// Check headers for cache hit (could be redis_hit or semantic_hit)
	cacheHeader := resp2.Header.Get("X-Gateway-Cache")
	assert.True(t, cacheHeader == "redis_hit" || cacheHeader == "semantic_hit", "Expected cache hit, got %s", cacheHeader)
}
