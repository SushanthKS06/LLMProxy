-- File: internal/db/migrations/002_add_cost_saved.sql
-- WHY: Distinguish between actual spend and money saved from cache hits.

ALTER TABLE usage_log 
ADD COLUMN IF NOT EXISTS cost_saved_usd NUMERIC(10,8) NOT NULL DEFAULT 0.0 CHECK (cost_saved_usd >= 0);
