#!/usr/bin/env python3
# File: scripts/seed_demo_data.py
# Purpose: Populate usage_log with 30 days of realistic demo data.
# Run before doing a demo or recording a video walkthrough.

import asyncio
import asyncpg
import random
import os
from datetime import datetime, timedelta, timezone
from decimal import Decimal

MODELS = {
    "gpt-4o-mini":          {"input": 0.00000015, "output": 0.0000006},
    "claude-haiku-3":       {"input": 0.00000025, "output": 0.00000125},
    "claude-sonnet-4":      {"input": 0.000003,   "output": 0.000015},
    "gpt-4o":               {"input": 0.0000025,  "output": 0.00001},
}
TEAMS        = ["team-alpha", "team-beta", "team-gamma", "default"]
FEATURES     = ["chat", "search", "summarize", "classify", "embedding", "codegen"]
COMPLEXITIES = ["simple", "medium", "complex"]
PROVIDERS    = ["openai", "anthropic"]

async def main():
    dsn = os.environ.get("POSTGRES_DSN",
          "postgres://gateway:gateway@localhost:5432/llmgateway")
    conn = await asyncpg.connect(dsn)

    rows = []
    now = datetime.now(timezone.utc)
    for day_offset in range(30):
        day = now - timedelta(days=day_offset)
        daily_count = random.randint(800, 2000)
        for _ in range(daily_count):
            model     = random.choices(list(MODELS.keys()), weights=[40,30,20,10])[0]
            complexity= random.choices(COMPLEXITIES, weights=[40,40,20])[0]
            cache_hit = random.random() < (0.44 if complexity == "simple" else 0.25)
            inp_tok   = random.randint(10, 400)
            out_tok   = random.randint(20, 300)
            cost      = (inp_tok * MODELS[model]["input"] +
                         out_tok * MODELS[model]["output"])
            latency   = random.randint(5, 20) if cache_hit else random.randint(200, 2500)
            ts        = day - timedelta(seconds=random.randint(0, 86400))
            rows.append((
                ts, random.choice(TEAMS), random.choice(FEATURES),
                model, "anthropic" if "claude" in model else "openai",
                inp_tok, out_tok, round(cost, 8), latency, cache_hit, complexity
            ))

    await conn.executemany("""
        INSERT INTO usage_log
          (created_at, team_id, feature_tag, model_used, provider,
           input_tokens, output_tokens, cost_usd, latency_ms, cache_hit, complexity)
        VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
    """, rows)

    await conn.close()
    print(f"Seeded {len(rows)} usage events across 30 days.")

if __name__ == "__main__":
    asyncio.run(main())
