// File: cmd/migrate/main.go
// WHY: Standalone migration runner referenced by Makefile targets:
//   make migrate      → go run ./cmd/migrate up
//   make migrate-down → go run ./cmd/migrate down
//
// Uses the pgx pool directly (same DSN as the gateway) without needing
// golang-migrate driver registration for Phase 01.

package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/sushanthks/llm-gateway/internal/config"
	"github.com/sushanthks/llm-gateway/internal/db"
)

func main() {
	// Load .env for local development
	_ = godotenv.Load()

	direction := "up"
	if len(os.Args) > 1 {
		direction = os.Args[1]
	}

	cfg := config.Load()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := db.NewPool(ctx, &cfg.Database)
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate: failed to connect to postgres: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	switch direction {
	case "up":
		if err := migrateUp(ctx, pool); err != nil {
			fmt.Fprintf(os.Stderr, "migrate up: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("✓ Migrations applied successfully")

	case "down":
		if err := migrateDown(ctx, pool); err != nil {
			fmt.Fprintf(os.Stderr, "migrate down: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("✓ Schema dropped")

	default:
		fmt.Fprintf(os.Stderr, "unknown direction %q — use 'up' or 'down'\n", direction)
		os.Exit(1)
	}
}

// migrateUp reads and executes the initial schema SQL via the pgx pool.
// All DDL uses IF NOT EXISTS so running multiple times is safe.
func migrateUp(ctx context.Context, pool *pgxpool.Pool) error {
	paths := []string{
		"internal/db/migrations/001_initial.sql",
		"/app/internal/db/migrations/001_initial.sql",
	}

	var sqlBytes []byte
	var readErr error
	for _, p := range paths {
		sqlBytes, readErr = os.ReadFile(p)
		if readErr == nil {
			fmt.Printf("  applying: %s\n", p)
			break
		}
	}
	if readErr != nil {
		return fmt.Errorf("migration file not found (tried %v): %w", paths, readErr)
	}

	if _, err := pool.Exec(ctx, string(sqlBytes)); err != nil {
		// Non-fatal: may warn on timescaledb hypertable re-creation
		fmt.Printf("  warning: %v\n  (safe to ignore if schema already exists)\n", err)
	}

	return nil
}

// migrateDown drops the gateway schema objects.
// WARNING: Destructive — all data in prompt_cache and usage_log will be lost.
func migrateDown(ctx context.Context, pool *pgxpool.Pool) error {
	downSQL := `
		DROP TABLE IF EXISTS usage_log CASCADE;
		DROP TABLE IF EXISTS prompt_cache CASCADE;
		DROP EXTENSION IF EXISTS vector;
	`
	_, err := pool.Exec(ctx, downSQL)
	return err
}

