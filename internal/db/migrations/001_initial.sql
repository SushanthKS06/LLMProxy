-- File: internal/db/migrations/001_initial.sql
-- WHY: pgvector extension must be created before the vector column.
--      TimescaleDB extension converts usage_log to a hypertable for
--      efficient time-series queries (10x faster range scans vs plain PG).

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS timescaledb CASCADE;

-- Prompt cache table for semantic caching
CREATE TABLE IF NOT EXISTS prompt_cache (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    prompt_hash   TEXT        NOT NULL,
    prompt_text   TEXT        NOT NULL,
    embedding     vector(1536) NOT NULL,
    response_json JSONB       NOT NULL,
    model_used    TEXT        NOT NULL,
    input_tokens  INT         NOT NULL CHECK (input_tokens >= 0),
    output_tokens INT         NOT NULL CHECK (output_tokens >= 0),
    cost_usd      NUMERIC(10,8) NOT NULL CHECK (cost_usd >= 0),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    hit_count     INT         NOT NULL DEFAULT 0,
    team_id       TEXT        NOT NULL DEFAULT 'default'
);

-- IVFFlat index for approximate nearest neighbor search.
-- WHY lists=100: rule of thumb is sqrt(rows) for tables up to 1M rows.
-- IMPORTANT: run ANALYZE prompt_cache after loading initial data to
-- let the planner use this index correctly.
CREATE INDEX IF NOT EXISTS prompt_cache_embedding_idx
    ON prompt_cache USING ivfflat (embedding vector_cosine_ops)
    WITH (lists = 100);

CREATE INDEX IF NOT EXISTS prompt_cache_team_created_idx
    ON prompt_cache (team_id, created_at DESC);

CREATE INDEX IF NOT EXISTS prompt_cache_hash_idx
    ON prompt_cache (prompt_hash);

-- Usage log table for cost tracking (TimescaleDB hypertable)
CREATE TABLE IF NOT EXISTS usage_log (
    id            BIGSERIAL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    team_id       TEXT        NOT NULL,
    feature_tag   TEXT        NOT NULL DEFAULT 'untagged',
    model_used    TEXT        NOT NULL,
    provider      TEXT        NOT NULL,
    input_tokens  INT         NOT NULL CHECK (input_tokens >= 0),
    output_tokens INT         NOT NULL CHECK (output_tokens >= 0),
    cost_usd      NUMERIC(10,8) NOT NULL CHECK (cost_usd >= 0),
    latency_ms    INT         NOT NULL CHECK (latency_ms >= 0),
    cache_hit     BOOLEAN     NOT NULL DEFAULT FALSE,
    complexity    TEXT        NOT NULL DEFAULT 'medium'
                              CHECK (complexity IN ('simple','medium','complex'))
);

-- Convert to TimescaleDB hypertable for efficient time-series queries
SELECT create_hypertable('usage_log', 'created_at', if_not_exists => TRUE);

CREATE INDEX IF NOT EXISTS usage_log_team_time_idx
    ON usage_log (team_id, created_at DESC);

CREATE INDEX IF NOT EXISTS usage_log_cache_hit_idx
    ON usage_log (cache_hit, created_at DESC);
