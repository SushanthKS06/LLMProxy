// File: internal/cache/semantic_cache.go
// WHY: 2-layer semantic cache: Redis for exact hash lookup (<1ms),
// pgvector for semantic similarity search. Near-misses (0.85-0.92) are
// logged but NOT served to prevent intent-mismatch false positives.
//
// FIX (ISSUE-04): Both INSERT statements now include ON CONFLICT DO NOTHING
//   so concurrent cache-miss requests storing the same prompt produce exactly
//   one row instead of duplicate rows that inflate hit counts and ANN scan cost.
// FIX (ISSUE-08): All fmt.Printf error paths replaced with slog.Default().

package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	pgvector "github.com/pgvector/pgvector-go"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/redis/go-redis/v9"
	"github.com/sushanthks/llm-gateway/internal/config"
	"github.com/sushanthks/llm-gateway/internal/db"
)

var (
	cacheHitsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_cache_hits_total",
		Help: "Total number of cache hits",
	}, []string{"team", "type"})

	cacheMissesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_cache_misses_total",
		Help: "Total number of cache misses",
	}, []string{"team"})

	cacheNearMissesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_cache_near_misses_total",
		Help: "Total number of near-misses (similarity 0.85-0.92)",
	}, []string{"team", "reason"})

	cacheLookupLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gateway_cache_lookup_latency_ms",
		Help:    "Latency of cache lookups in milliseconds",
		Buckets: []float64{1, 2, 5, 10, 25, 50, 100, 250, 500},
	}, []string{"type"})
)

// CacheResult represents a cached response.
// CacheID is the PostgreSQL UUID stored as a string for portability.
type CacheResult struct {
	ResponseJSON string
	ModelUsed    string
	CostUSD      float64
	Similarity   float64
	CacheID      string // UUID stored as string (avoids external uuid dependency)
}

// CacheStoreRequest represents a request to store in cache.
type CacheStoreRequest struct {
	PromptHash   string
	PromptText   string
	Embedding    []float32
	ResponseJSON string
	ModelUsed    string
	InputTokens  int
	OutputTokens int
	CostUSD      float64
	TeamID       string
}

// SemanticCache handles semantic caching with 2-layer lookup.
type SemanticCache struct {
	pool     *pgxpool.Pool
	redis    *redis.Client
	embedder *Embedder
	config   *config.RoutingConfig
}

// NewSemanticCache creates a new SemanticCache instance.
func NewSemanticCache(pool *pgxpool.Pool, redisClient *redis.Client, embedder *Embedder, cfg *config.RoutingConfig) *SemanticCache {
	return &SemanticCache{
		pool:     pool,
		redis:    redisClient,
		embedder: embedder,
		config:   cfg,
	}
}

// Lookup performs a 2-layer cache lookup.
// Layer 1: Exact hash match in Redis (<1ms)
// Layer 2: Semantic similarity search in pgvector
func (sc *SemanticCache) Lookup(ctx context.Context, prompt string, teamID string) (*CacheResult, error) {
	start := time.Now()
	defer func() {
		cacheLookupLatency.WithLabelValues("total").Observe(float64(time.Since(start).Milliseconds()))
	}()

	// Step 1: Hash prompt and check Redis for exact match
	hash := hashText(prompt)
	redisKey := fmt.Sprintf("cache:%s:%s", teamID, hash)

	layer1Start := time.Now()
	cached, err := db.Get(ctx, sc.redis, redisKey)
	cacheLookupLatency.WithLabelValues("layer1_redis").Observe(float64(time.Since(layer1Start).Milliseconds()))

	if err == nil && cached != "" {
		// Layer 1 hit - exact hash match
		cacheHitsTotal.WithLabelValues(teamID, "exact_hash").Inc()
		cacheLookupLatency.WithLabelValues("layer1_hit").Observe(float64(time.Since(start).Milliseconds()))

		var result CacheResult
		if err := json.Unmarshal([]byte(cached), &result); err != nil {
			return nil, fmt.Errorf("failed to unmarshal cached result: %w", err)
		}

		// Update hit count in database (best-effort, non-blocking)
		go sc.incrementHitCount(context.Background(), result.CacheID)

		return &result, nil
	}

	// Layer 1 miss - proceed to semantic search
	cacheMissesTotal.WithLabelValues(teamID).Inc()

	// Step 2: Get embedding (checks Redis cache first via Embedder)
	embedding, err := sc.embedder.EmbedSingle(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("failed to get embedding: %w", err)
	}

	// Step 3: pgvector similarity search
	layer2Start := time.Now()
	result, err := sc.semanticSearch(ctx, embedding, teamID)
	cacheLookupLatency.WithLabelValues("layer2_pgvector").Observe(float64(time.Since(layer2Start).Milliseconds()))

	if err != nil {
		return nil, fmt.Errorf("semantic search failed: %w", err)
	}

	if result == nil {
		return nil, nil // Cache miss
	}

	// Check similarity threshold
	similarity := result.Similarity

	if similarity >= sc.config.SimilarityThreshold {
		// Cache hit - similarity >= 0.92
		cacheHitsTotal.WithLabelValues(teamID, "semantic").Inc()
		cacheLookupLatency.WithLabelValues("semantic_hit").Observe(float64(time.Since(start).Milliseconds()))

		// Promote to Redis for fast exact lookup next time
		data, _ := json.Marshal(result)
		_ = db.Set(ctx, sc.redis, redisKey, string(data), 24*time.Hour)

		// Update hit count (best-effort)
		go sc.incrementHitCount(context.Background(), result.CacheID)

		return result, nil
	}

	// Near-miss: similarity 0.85-0.92
	// WHY: Prompts can be semantically similar but have different INTENT.
	// Example: "summarize this" vs "critique this" on the same text score ~0.87.
	// We reject anything below 0.92 to prevent serving wrong responses.
	if similarity >= 0.85 && similarity < sc.config.SimilarityThreshold {
		cacheNearMissesTotal.WithLabelValues(teamID, "intent_mismatch").Inc()
		cacheLookupLatency.WithLabelValues("near_miss").Observe(float64(time.Since(start).Milliseconds()))
		return nil, nil // Don't serve cache for near-miss
	}

	// Below 0.85 - clear miss
	return nil, nil
}

// semanticSearch performs pgvector cosine-similarity search.
func (sc *SemanticCache) semanticSearch(ctx context.Context, embedding []float32, teamID string) (*CacheResult, error) {
	// FIX (P2-3): LIMIT changed from 5 → 1. The caller only reads the first row;
	// fetching 5 rows wastes DB I/O and network bandwidth on every semantic search.
	query := `
		SELECT id::text, response_json, model_used, cost_usd, 1 - (embedding <=> $1) as similarity
		FROM prompt_cache
		WHERE team_id = $2
		ORDER BY embedding <=> $1
		LIMIT 1
	`

	vector := pgvector.NewVector(embedding)
	rows, err := sc.pool.Query(ctx, query, vector, teamID)
	if err != nil {
		return nil, fmt.Errorf("failed to query semantic cache: %w", err)
	}
	defer rows.Close()

	if rows.Next() {
		var id string
		var responseJSON string
		var modelUsed string
		var costUSD float64
		var similarity float64

		if err := rows.Scan(&id, &responseJSON, &modelUsed, &costUSD, &similarity); err != nil {
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}

		return &CacheResult{
			ResponseJSON: responseJSON,
			ModelUsed:    modelUsed,
			CostUSD:      costUSD,
			Similarity:   similarity,
			CacheID:      id,
		}, nil
	}

	return nil, nil
}

// Store stores a pre-built CacheStoreRequest in the semantic cache (async).
// This is a low-level method; prefer StoreResponse for most use cases.
func (sc *SemanticCache) Store(ctx context.Context, req *CacheStoreRequest) error {
	// Run asynchronously to not block the response
	go func() {
		storeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		// FIX (ISSUE-04): ON CONFLICT DO NOTHING prevents duplicate rows when two
		// concurrent requests race to cache the same prompt.
		query := `
			INSERT INTO prompt_cache 
				(prompt_hash, prompt_text, embedding, response_json, model_used, 
				 input_tokens, output_tokens, cost_usd, team_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (prompt_hash, team_id) DO NOTHING
		`

		vector := pgvector.NewVector(req.Embedding)
		_, err := sc.pool.Exec(storeCtx, query,
			req.PromptHash,
			req.PromptText,
			vector,
			req.ResponseJSON,
			req.ModelUsed,
			req.InputTokens,
			req.OutputTokens,
			req.CostUSD,
			req.TeamID,
		)

		if err != nil {
			// FIX (ISSUE-08): structured log instead of fmt.Printf
			slog.Default().Error("cache: failed to store in postgres", "error", err)
			return
		}

		// Also populate Redis for fast exact lookup on next identical request
		hash := hashText(req.PromptText)
		redisKey := fmt.Sprintf("cache:%s:%s", req.TeamID, hash)
		result := &CacheResult{
			ResponseJSON: req.ResponseJSON,
			ModelUsed:    req.ModelUsed,
			CostUSD:      req.CostUSD,
			Similarity:   1.0,
		}
		data, err := json.Marshal(result)
		if err == nil {
			_ = db.Set(storeCtx, sc.redis, redisKey, string(data), 24*time.Hour)
		}
	}()

	return nil
}

// StoreResponse is the primary method to persist an LLM response to the semantic cache.
// It automatically generates the embedding for the prompt, then asynchronously
// writes to PostgreSQL and Redis. Fire-and-forget; errors are only logged.
//
// WHY this approach: embedding generation takes 50-100ms (batched OpenAI call).
// We cannot block the HTTP response path. The goroutine handles the async work
// and any failure is non-fatal — cache write failure just means a future cache miss.
func (sc *SemanticCache) StoreResponse(ctx context.Context, promptText, responseJSON, model string, inputTokens, outputTokens int, cost float64, teamID string) {
	go func() {
		storeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Step 1: Generate (or retrieve from Redis cache) the embedding
		embedding, err := sc.embedder.EmbedSingle(storeCtx, promptText)
		if err != nil {
			// FIX (ISSUE-08): structured log instead of fmt.Printf
			slog.Default().Error("cache: failed to embed prompt for storage", "error", err)
			return
		}

		// Step 2: Write to PostgreSQL with the vector.
		// FIX (ISSUE-04): ON CONFLICT DO NOTHING prevents duplicate rows when two
		// concurrent cache-miss requests for the same prompt both try to store.
		query := `
			INSERT INTO prompt_cache 
				(prompt_hash, prompt_text, embedding, response_json, model_used, 
				 input_tokens, output_tokens, cost_usd, team_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (prompt_hash, team_id) DO NOTHING
		`
		vector := pgvector.NewVector(embedding)
		_, err = sc.pool.Exec(storeCtx, query,
			hashText(promptText),
			promptText,
			vector,
			responseJSON,
			model,
			inputTokens,
			outputTokens,
			cost,
			teamID,
		)
		if err != nil {
			// FIX (ISSUE-08): structured log instead of fmt.Printf
			slog.Default().Error("cache: failed to store response in postgres", "error", err)
			return
		}

		// Step 3: Populate Redis for instant exact-hash hit on the next identical request
		hash := hashText(promptText)
		redisKey := fmt.Sprintf("cache:%s:%s", teamID, hash)
		result := &CacheResult{
			ResponseJSON: responseJSON,
			ModelUsed:    model,
			CostUSD:      cost,
			Similarity:   1.0,
		}
		data, err := json.Marshal(result)
		if err == nil {
			_ = db.Set(storeCtx, sc.redis, redisKey, string(data), 24*time.Hour)
		}
	}()
}

// incrementHitCount increments the hit count for a cached entry (best-effort).
// FIX (P2-4): Use native UUID comparison ($1::uuid) instead of id::text = $1.
// Casting every stored UUID to TEXT on the right side makes the primary key
// index unusable, causing a full sequential scan on every cache hit.
func (sc *SemanticCache) incrementHitCount(ctx context.Context, cacheID string) {
	query := `UPDATE prompt_cache SET hit_count = hit_count + 1 WHERE id = $1::uuid`
	_, err := sc.pool.Exec(ctx, query, cacheID)
	if err != nil {
		// FIX (ISSUE-08): structured log instead of fmt.Printf
		slog.Default().Error("cache: failed to increment hit count", "error", err, "cache_id", cacheID)
	}
}

// GetPromptHash returns the SHA256 hex hash of a prompt.
func GetPromptHash(prompt string) string {
	return hashText(prompt)
}

// hashText generates SHA256 hash of text (unexported, used internally).
// NOTE: This is the canonical copy; embedder.go previously had a duplicate
// that has been removed (FIX BUG-01).
func hashText(text string) string {
	h := sha256.Sum256([]byte(text))
	return hex.EncodeToString(h[:])
}
