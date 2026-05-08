// File: internal/db/postgres.go
// WHY: PostgreSQL connection pool with pgx/v5 for high-performance database access.
// Uses connection pooling to handle concurrent requests efficiently.

package db

import (
	"context"
	"fmt"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sushanthks/llm-gateway/internal/config"
)

// NewPool creates a new pgx connection pool.
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

// HealthCheck verifies the database connection is healthy.
func HealthCheck(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return fmt.Errorf("pool is nil")
	}
	return pool.Ping(ctx)
}

// RunMigrations runs database migrations using golang-migrate.
// WHY: Migrations must be run on startup to ensure schema is up to date.
func RunMigrations(ctx context.Context, dsn string, migrationsPath string) error {
	m, err := migrate.New(migrationsPath, dsn)
	if err != nil {
		return fmt.Errorf("failed to create migration: %w", err)
	}

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("migration failed: %w", err)
	}

	return nil
}

// GetPoolConfig returns the default pool configuration.
func GetPoolConfig(dsn string) (*pgxpool.Config, error) {
	return pgxpool.ParseConfig(dsn)
}

// QueryRow wraps pgxpool.QueryRow for convenience.
func QueryRow(ctx context.Context, pool *pgxpool.Pool, sql string, args ...interface{}) (pgx.Row, error) {
	return pool.QueryRow(ctx, sql, args...), nil
}

// Query wraps pgxpool.Query for convenience.
func Query(ctx context.Context, pool *pgxpool.Pool, sql string, args ...interface{}) (pgx.Rows, error) {
	return pool.Query(ctx, sql, args...)
}

// Exec wraps pgxpool.Exec for convenience.
func Exec(ctx context.Context, pool *pgxpool.Pool, sql string, args ...interface{}) (pgconn.CommandTag, error) {
	return pool.Exec(ctx, sql, args...)
}
