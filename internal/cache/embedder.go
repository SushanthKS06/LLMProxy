// File: internal/cache/embedder.go
// WHY: OpenAI embedding service with async batching to reduce API costs.
// Batches up to 20 prompts for 50ms before firing a single API call.

package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/redis/go-redis/v9"
	"github.com/sashabaranov/go-openai"
	"github.com/sushanthks/llm-gateway/internal/config"
	"github.com/sushanthks/llm-gateway/internal/db"
)

var (
	embeddingLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gateway_embedding_latency_ms",
		Help:    "Latency of embedding API calls in milliseconds",
		Buckets: []float64{10, 25, 50, 100, 200, 500, 1000},
	}, []string{"cache_hit"})

	embeddingRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_embedding_requests_total",
		Help: "Total number of embedding requests",
	}, []string{"cache_hit"})
)

// Embedder handles OpenAI embedding generation with caching and batching.
type Embedder struct {
	client       *openai.Client
	redis        *redis.Client
	config       *config.EmbeddingsConfig
	batchCh      chan string
	batchResults map[string]chan []float32
	mu           sync.Mutex
	wg           sync.WaitGroup
	ctx          context.Context
	cancel       context.CancelFunc
}

// NewEmbedder creates a new Embedder instance.
func NewEmbedder(cfg *config.Config, redisClient *redis.Client) *Embedder {
	client := openai.NewClient(cfg.Providers.OpenAIAPIKey)
	ctx, cancel := context.WithCancel(context.Background())

	e := &Embedder{
		client:       client,
		redis:        redisClient,
		config:       &cfg.Embeddings,
		batchCh:      make(chan string, 1000),
		batchResults: make(map[string]chan []float32),
		ctx:          ctx,
		cancel:       cancel,
	}

	// Start batch processor
	e.wg.Add(1)
	go e.processBatch()

	return e
}

// EmbedSingle generates embedding for a single text.
// WHY: Checks Redis cache first to avoid re-embedding the same prompt.
func (e *Embedder) EmbedSingle(ctx context.Context, text string) ([]float32, error) {
	// Check Redis cache first
	hash := hashText(text)
	cacheKey := fmt.Sprintf("emb:%s", hash)

	// Try to get from Redis cache
	cached, err := db.Get(ctx, e.redis, cacheKey)
	if err == nil && cached != "" {
		embeddingRequests.WithLabelValues("hit").Inc()
		return parseEmbedding(cached), nil
	}

	// Cache miss - generate embedding
	embeddingRequests.WithLabelValues("miss").Inc()

	// Use batch channel to get embedding
	resultCh := make(chan []float32, 1)
	e.mu.Lock()
	e.batchResults[hash] = resultCh
	e.mu.Unlock()

	select {
	case e.batchCh <- text:
		// Wait for result
		select {
		case result := <-resultCh:
			// Cache the result
			embStr := serializeEmbedding(result)
			_ = db.Set(ctx, e.redis, cacheKey, embStr, 24*time.Hour)
			return result, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(30 * time.Second):
			return nil, fmt.Errorf("embedding timeout")
		}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// EmbedBatch generates embeddings for multiple texts.
func (e *Embedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	results := make([][]float32, len(texts))
	pending := make([]int, 0)
	hashes := make([]string, len(texts))

	// Check cache for each text
	for i, text := range texts {
		hash := hashText(texts[i])
		hashes[i] = hash
		cacheKey := fmt.Sprintf("emb:%s", hash)

		cached, err := db.Get(ctx, e.redis, cacheKey)
		if err == nil && cached != "" {
			results[i] = parseEmbedding(cached)
		} else {
			pending = append(pending, i)
		}
	}

	// Batch request for uncached texts
	if len(pending) > 0 {
		uncached := make([]string, len(pending))
		for i, idx := range pending {
			uncached[i] = texts[idx]
		}

		embeddings, err := e.callOpenAI(ctx, uncached)
		if err != nil {
			return nil, err
		}

		// Update results and cache
		for i, idx := range pending {
			results[idx] = embeddings[i]
			// Cache the embedding
			cacheKey := fmt.Sprintf("emb:%s", hashes[idx])
			embStr := serializeEmbedding(embeddings[i])
			_ = db.Set(ctx, e.redis, cacheKey, embStr, 24*time.Hour)
		}
	}

	return results, nil
}

// processBatch handles async batching of embedding requests.
func (e *Embedder) processBatch() {
	defer e.wg.Done()

	ticker := time.NewTicker(time.Duration(e.config.BatchTimeoutMs) * time.Millisecond)
	defer ticker.Stop()

	batch := make([]string, 0, e.config.EmbeddingBatchSize)

	flush := func() {
		if len(batch) == 0 {
			return
		}

		ctx, cancel := context.WithTimeout(e.ctx, 30*time.Second)
		defer cancel()

		start := time.Now()
		embeddings, err := e.callOpenAI(ctx, batch)
		latency := time.Since(start).Milliseconds()

		if err != nil {
			// Send error to all waiting channels
			for _, text := range batch {
				hash := hashText(text)
				e.mu.Lock()
				if ch, ok := e.batchResults[hash]; ok {
					close(ch)
					delete(e.batchResults, hash)
				}
				e.mu.Unlock()
			}
		} else {
			// Send embeddings to waiting channels
			for i, text := range batch {
				hash := hashText(text)
				e.mu.Lock()
				if ch, ok := e.batchResults[hash]; ok {
					ch <- embeddings[i]
					close(ch)
					delete(e.batchResults, hash)
				}
				e.mu.Unlock()
			}
		}

		embeddingLatency.WithLabelValues("miss").Observe(float64(latency))
		batch = batch[:0]
	}

	for {
		select {
		case <-e.ctx.Done():
			flush()
			return
		case text := <-e.batchCh:
			batch = append(batch, text)
			if len(batch) >= e.config.EmbeddingBatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// callOpenAI makes the actual OpenAI API call.
func (e *Embedder) callOpenAI(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	req := openai.EmbeddingRequest{
		Model: e.config.EmbeddingModel,
		Input: texts,
	}

	resp, err := e.client.CreateEmbeddings(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to create embeddings: %w", err)
	}

	embeddings := make([][]float32, len(resp.Data))
	for i, data := range resp.Data {
		embeddings[i] = data.Embedding
	}

	return embeddings, nil
}

// Close shuts down the embedder.
func (e *Embedder) Close() error {
	e.cancel()
	e.wg.Wait()
	return nil
}

// hashText generates SHA256 hash of text.
func hashText(text string) string {
	h := sha256.Sum256([]byte(text))
	return hex.EncodeToString(h[:])
}

// serializeEmbedding converts float32 slice to string for Redis storage.
func serializeEmbedding(emb []float32) string {
	result := make([]byte, 0, len(emb)*4)
	for _, f := range emb {
		bits := math.Float32bits(f)
		result = append(result, byte(bits>>24), byte(bits>>16), byte(bits>>8), byte(bits))
	}
	return hex.EncodeToString(result)
}

// parseEmbedding converts stored string back to float32 slice.
func parseEmbedding(s string) []float32 {
	data, err := hex.DecodeString(s)
	if err != nil {
		return nil
	}
	result := make([]float32, len(data)/4)
	for i := 0; i < len(result); i++ {
		bits := uint32(data[i*4])<<24 | uint32(data[i*4+1])<<16 | uint32(data[i*4+2])<<8 | uint32(data[i*4+3])
		result[i] = math.Float32frombits(bits)
	}
	return result
}
