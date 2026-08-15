// File: internal/db/postgres.go
// WHY: PostgreSQL connection pool with pgx/v5 for high-performance database access.
// Uses connection pooling to handle concurrent requests efficiently.
//
// FIX (MINOR-03): Removed three unused wrapper functions (QueryRow, Query, Exec)
//   that thin-wrapped the pool methods. No caller in the project used them —
//   all callers call pool.Exec / pool.Query / pool.QueryRow directly.
//   Keeping dead wrappers creates confusion about which path to use.

package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sushanthks/llm-gateway/internal/config"
)

// NewPool creates a new pgx connection pool with production-ready settings.
func NewPool(ctx context.Context, cfg *config.DatabaseConfig) (*pgxpool.Pool, error) {
	poolConfig, err := pgxpool.ParseConfig(cfg.PostgresDSN)
	if err != nil {
		return nil, fmt.Errorf("failed to parse postgres dsn: %w", err)
	}

	// Configure connection pool
	poolConfig.MaxConns = cfg.MaxConns
	poolConfig.MinConns = cfg.MinConns
	poolConfig.MaxConnLifetime = time.Hour
	poolConfig.MaxConnIdleTime = 30 * time.Minute
	poolConfig.HealthCheckPeriod = 30 * time.Second

	// Create the pool
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create connection pool: %w", err)
	}

	// Verify connection
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	return pool, nil
}

// PostgresHealthCheck verifies the PostgreSQL connection pool is healthy.
func PostgresHealthCheck(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return fmt.Errorf("pool is nil")
	}
	return pool.Ping(ctx)
}
