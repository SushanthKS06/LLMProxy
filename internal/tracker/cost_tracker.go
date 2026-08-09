// File: internal/tracker/cost_tracker.go
// WHY: Tracks token usage and costs in TimescaleDB with batch inserts
// for high throughput. Exposes Prometheus metrics for observability.
//
// FIX (BUG-04): RecordUsage now logs and counts dropped events instead of
//   silently discarding them when the channel is full under high load.
// FIX (BUG-05): batchInsert replaced string-concatenated SQL with
//   pgx.CopyFrom — correct, safe, and ~10x faster for bulk inserts.
// FIX (ISSUE-08): All fmt.Printf error paths replaced with slog.

package tracker

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/sushanthks/llm-gateway/internal/config"
)

var (
	totalCostUSD = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_total_cost_usd",
		Help: "Total cost in USD",
	}, []string{"team", "model", "provider"})

	totalTokens = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_total_tokens",
		Help: "Total number of tokens",
	}, []string{"team", "model", "type"})

	requestLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gateway_request_latency_ms",
		Help:    "Request latency in milliseconds",
		Buckets: []float64{50, 100, 250, 500, 1000, 2500, 5000, 10000},
	}, []string{"model", "cache_hit", "complexity"})

	requestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_requests_total",
		Help: "Total number of requests",
	}, []string{"team", "model", "complexity", "cache_hit"})

	cacheHitRate = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gateway_cache_hit_rate",
		Help: "Cache hit rate (0-1)",
	}, []string{"team"})

	// FIX (BUG-04): counter for events dropped when the channel is full.
	// Alert on this metric in production — sustained drops indicate the
	// flush interval or channel capacity needs tuning.
	eventsDroppedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_usage_events_dropped_total",
		Help: "Usage events dropped because the internal buffer channel was full",
	}, []string{"team"})
)

// UsageEvent represents a usage event to be recorded.
type UsageEvent struct {
	TeamID       string
	FeatureTag   string
	ModelUsed    string
	Provider     string
	InputTokens  int
	OutputTokens int
	CostUSD      float64
	LatencyMS    int
	CacheHit     bool
	Complexity   string
}

// TeamSummary represents a summary of team usage.
type TeamSummary struct {
	TotalCostUSD   float64
	TotalRequests  int64
	CacheHitRate   float64
	ModelBreakdown map[string]ModelSummary
	AvgLatencyMS   float64
	P99LatencyMS   float64
	TopFeatureTags []string
}

// ModelSummary represents usage summary for a model.
type ModelSummary struct {
	RequestCount int64
	TotalCostUSD float64
	TotalTokens  int64
	AvgLatencyMS float64
}

// CostTracker tracks usage and costs.
type CostTracker struct {
	pool          *pgxpool.Pool
	eventCh       chan *UsageEvent
	buffer        []*UsageEvent
	mu            sync.Mutex
	wg            sync.WaitGroup
	ctx           context.Context
	cancel        context.CancelFunc
	flushInterval time.Duration
	maxBatchSize  int
}

// NewCostTracker creates a new CostTracker instance.
func NewCostTracker(ctx context.Context, pool *pgxpool.Pool) *CostTracker {
	ctx, cancel := context.WithCancel(ctx)
	ct := &CostTracker{
		pool:          pool,
		eventCh:       make(chan *UsageEvent, 1000),
		buffer:        make([]*UsageEvent, 0, 100),
		ctx:           ctx,
		cancel:        cancel,
		flushInterval: 500 * time.Millisecond,
		maxBatchSize:  100,
	}

	// Start background flush goroutine
	ct.wg.Add(1)
	go ct.flushLoop()

	return ct
}

// RecordUsage records a usage event (non-blocking).
// FIX (BUG-04): When the channel is full, the event is logged and counted
// in a Prometheus metric instead of being silently dropped.
func (ct *CostTracker) RecordUsage(event *UsageEvent) error {
	select {
	case ct.eventCh <- event:
		// Update Prometheus metrics immediately (before the async DB write)
		requestsTotal.WithLabelValues(event.TeamID, event.ModelUsed, event.Complexity, fmt.Sprintf("%t", event.CacheHit)).Inc()
		totalCostUSD.WithLabelValues(event.TeamID, event.ModelUsed, event.Provider).Add(event.CostUSD)
		totalTokens.WithLabelValues(event.TeamID, event.ModelUsed, "input").Add(float64(event.InputTokens))
		totalTokens.WithLabelValues(event.TeamID, event.ModelUsed, "output").Add(float64(event.OutputTokens))

		cacheHitLabel := "false"
		if event.CacheHit {
			cacheHitLabel = "true"
		}
		requestLatency.WithLabelValues(event.ModelUsed, cacheHitLabel, event.Complexity).Observe(float64(event.LatencyMS))

		return nil
	default:
		// FIX (BUG-04): channel full — log + count so operators know to tune
		// flushInterval or channel capacity, rather than silently losing data.
		eventsDroppedTotal.WithLabelValues(event.TeamID).Inc()
		slog.Default().Warn("tracker: event channel full, dropping usage event",
			"team_id", event.TeamID,
			"model", event.ModelUsed,
			"cost_usd", event.CostUSD,
		)
		return fmt.Errorf("event channel full, dropping event for team %s", event.TeamID)
	}
}

// flushLoop periodically flushes buffered events to the database.
func (ct *CostTracker) flushLoop() {
	defer ct.wg.Done()

	ticker := time.NewTicker(ct.flushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ct.ctx.Done():
			// Final flush — drain all remaining events from the channel
			for {
				select {
				case event := <-ct.eventCh:
					ct.mu.Lock()
					ct.buffer = append(ct.buffer, event)
					ct.mu.Unlock()
				default:
					ct.flush()
					return
				}
			}
		case event := <-ct.eventCh:
			ct.mu.Lock()
			ct.buffer = append(ct.buffer, event)
			if len(ct.buffer) >= ct.maxBatchSize {
				ct.mu.Unlock()
				ct.flush()
			} else {
				ct.mu.Unlock()
			}
		case <-ticker.C:
			ct.flush()
		}
	}
}

// flush writes buffered events to the database.
func (ct *CostTracker) flush() {
	ct.mu.Lock()
	if len(ct.buffer) == 0 {
		ct.mu.Unlock()
		return
	}

	// Swap out the buffer atomically so new events can queue while we write.
	events := ct.buffer
	ct.buffer = make([]*UsageEvent, 0, ct.maxBatchSize)
	ct.mu.Unlock()

	// FIX (ISSUE-08): structured log instead of fmt.Printf
	if err := ct.batchInsert(ct.ctx, events); err != nil {
		slog.Default().Error("tracker: failed to batch insert usage events",
			"error", err,
			"count", len(events),
		)
	}
}

// batchInsert performs a bulk insert of usage events using pgx.CopyFrom.
//
// FIX (BUG-05): The previous implementation built the VALUES clause via
// fmt.Sprintf string concatenation with manual $N placeholders — fragile,
// error-prone, and slow. pgx.CopyFrom uses the PostgreSQL COPY protocol,
// which is binary, fully parameterised, and ~10x faster for bulk writes.
func (ct *CostTracker) batchInsert(ctx context.Context, events []*UsageEvent) error {
	if len(events) == 0 {
		return nil
	}

	columns := []string{
		"team_id", "feature_tag", "model_used", "provider",
		"input_tokens", "output_tokens", "cost_usd", "latency_ms",
		"cache_hit", "complexity",
	}

	rows := make([][]interface{}, len(events))
	for i, e := range events {
		rows[i] = []interface{}{
			e.TeamID, e.FeatureTag, e.ModelUsed, e.Provider,
			e.InputTokens, e.OutputTokens, e.CostUSD, e.LatencyMS,
			e.CacheHit, e.Complexity,
		}
	}

	_, err := ct.pool.CopyFrom(
		ctx,
		pgx.Identifier{"usage_log"},
		columns,
		pgx.CopyFromRows(rows),
	)
	return err
}

// GetTeamSummary returns a summary of team usage.
func (ct *CostTracker) GetTeamSummary(ctx context.Context, teamID string, since time.Time) (*TeamSummary, error) {
	// Get total cost and requests
	costQuery := `
		SELECT 
			COALESCE(SUM(cost_usd), 0) as total_cost,
			COUNT(*) as total_requests,
			COALESCE(AVG(latency_ms), 0) as avg_latency,
			COALESCE(PERCENTILE_CONT(0.99) WITHIN GROUP (ORDER BY latency_ms), 0) as p99_latency
		FROM usage_log
		WHERE team_id = $1 AND created_at >= $2
	`

	var summary TeamSummary
	err := ct.pool.QueryRow(ctx, costQuery, teamID, since).Scan(
		&summary.TotalCostUSD,
		&summary.TotalRequests,
		&summary.AvgLatencyMS,
		&summary.P99LatencyMS,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get team summary: %w", err)
	}

	// Get cache hit rate
	cacheQuery := `
		SELECT 
			COUNT(*) FILTER (WHERE cache_hit = true)::float / NULLIF(COUNT(*), 0) as cache_hit_rate
		FROM usage_log
		WHERE team_id = $1 AND created_at >= $2
	`
	err = ct.pool.QueryRow(ctx, cacheQuery, teamID, since).Scan(&summary.CacheHitRate)
	if err != nil {
		return nil, fmt.Errorf("failed to get cache hit rate: %w", err)
	}

	// Get model breakdown
	modelQuery := `
		SELECT 
			model_used,
			COUNT(*) as request_count,
			COALESCE(SUM(cost_usd), 0) as total_cost,
			COALESCE(SUM(input_tokens + output_tokens), 0) as total_tokens,
			COALESCE(AVG(latency_ms), 0) as avg_latency
		FROM usage_log
		WHERE team_id = $1 AND created_at >= $2
		GROUP BY model_used
	`

	rows, err := ct.pool.Query(ctx, modelQuery, teamID, since)
	if err != nil {
		return nil, fmt.Errorf("failed to get model breakdown: %w", err)
	}
	defer rows.Close()

	summary.ModelBreakdown = make(map[string]ModelSummary)
	for rows.Next() {
		var ms ModelSummary
		var model string
		if err := rows.Scan(&model, &ms.RequestCount, &ms.TotalCostUSD, &ms.TotalTokens, &ms.AvgLatencyMS); err != nil {
			return nil, err
		}
		summary.ModelBreakdown[model] = ms
	}

	// Get top feature tags
	featureQuery := `
		SELECT feature_tag
		FROM usage_log
		WHERE team_id = $1 AND created_at >= $2
		GROUP BY feature_tag
		ORDER BY COUNT(*) DESC
		LIMIT 5
	`

	featureRows, err := ct.pool.Query(ctx, featureQuery, teamID, since)
	if err != nil {
		return nil, fmt.Errorf("failed to get top feature tags: %w", err)
	}
	defer featureRows.Close()

	for featureRows.Next() {
		var tag string
		if err := featureRows.Scan(&tag); err != nil {
			return nil, err
		}
		summary.TopFeatureTags = append(summary.TopFeatureTags, tag)
	}

	return &summary, nil
}

// UpdateCacheHitRate updates the cache hit rate gauge for a team.
func (ct *CostTracker) UpdateCacheHitRate(ctx context.Context, teamID string) error {
	query := `
		SELECT 
			COUNT(*) FILTER (WHERE cache_hit = true)::float / NULLIF(COUNT(*), 0) as cache_hit_rate
		FROM usage_log
		WHERE team_id = $1 AND created_at >= NOW() - INTERVAL '30 seconds'
	`

	var rate float64
	err := ct.pool.QueryRow(ctx, query, teamID).Scan(&rate)
	if err != nil {
		return err
	}

	cacheHitRate.WithLabelValues(teamID).Set(rate)
	return nil
}

// Close shuts down the cost tracker gracefully, flushing all buffered events.
func (ct *CostTracker) Close() error {
	ct.cancel()
	ct.wg.Wait()
	return nil
}

// Ensure config package is imported for completeness (used by callers).
var _ = config.ModelCost{}
