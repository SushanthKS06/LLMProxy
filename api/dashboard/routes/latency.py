# File: api/dashboard/routes/latency.py
# Latency percentiles endpoints

from fastapi import APIRouter, Query, Depends
from sqlalchemy.ext.asyncio import AsyncSession
from sqlalchemy import text
from datetime import datetime, timedelta

from database import get_db
from models.schemas import LatencyPercentiles

router = APIRouter(prefix="/api/v1/latency", tags=["latency"])


@router.get("/percentiles", response_model=LatencyPercentiles)
async def get_latency_percentiles(
    team_id: str = Query("default"),
    range: str = Query("24h"),
    db: AsyncSession = Depends(get_db),
):
    hours = parse_hours(range)
    since = datetime.utcnow() - timedelta(hours=hours)

    # Get overall percentiles
    percentiles_result = await db.execute(
        text("""
            SELECT 
                PERCENTILE_CONT(0.50) WITHIN GROUP (ORDER BY latency_ms) as p50,
                PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY latency_ms) as p95,
                PERCENTILE_CONT(0.99) WITHIN GROUP (ORDER BY latency_ms) as p99
            FROM usage_log
            WHERE team_id = :team_id AND created_at >= :since
        """),
        {"team_id": team_id, "since": since}
    )
    row = percentiles_result.fetchone()
    p50 = float(row[0]) if row and row[0] else 0.0
    p95 = float(row[1]) if row and row[1] else 0.0
    p99 = float(row[2]) if row and row[2] else 0.0

    # Get percentiles by model
    by_model_result = await db.execute(
        text("""
            SELECT 
                model_used,
                PERCENTILE_CONT(0.50) WITHIN GROUP (ORDER BY latency_ms) as p50,
                PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY latency_ms) as p95,
                PERCENTILE_CONT(0.99) WITHIN GROUP (ORDER BY latency_ms) as p99
            FROM usage_log
            WHERE team_id = :team_id AND created_at >= :since
            GROUP BY model_used
        """),
        {"team_id": team_id, "since": since}
    )

    by_model = {}
    for row in by_model_result.fetchall():
        model = row[0]
        by_model[model] = {
            "p50": float(row[1]) if row[1] else 0.0,
            "p95": float(row[2]) if row[2] else 0.0,
            "p99": float(row[3]) if row[3] else 0.0,
        }

    # Get percentiles by cache hit
    by_cache_hit_result = await db.execute(
        text("""
            SELECT 
                cache_hit,
                PERCENTILE_CONT(0.50) WITHIN GROUP (ORDER BY latency_ms) as p50,
                PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY latency_ms) as p95,
                PERCENTILE_CONT(0.99) WITHIN GROUP (ORDER BY latency_ms) as p99
            FROM usage_log
            WHERE team_id = :team_id AND created_at >= :since
            GROUP BY cache_hit
        """),
        {"team_id": team_id, "since": since}
    )

    by_cache_hit = {}
    for row in by_cache_hit_result.fetchall():
        cache_hit = "hit" if row[0] else "miss"
        by_cache_hit[cache_hit] = {
            "p50": float(row[1]) if row[1] else 0.0,
            "p95": float(row[2]) if row[2] else 0.0,
            "p99": float(row[3]) if row[3] else 0.0,
        }

    return LatencyPercentiles(
        p50_ms=p50,
        p95_ms=p95,
        p99_ms=p99,
        by_model=by_model,
        by_cache_hit=by_cache_hit,
    )


def parse_hours(range_str: str) -> int:
    """Parse range string to hours."""
    range_str = range_str.lower().strip()
    if range_str.endswith("h"):
        return int(range_str[:-1])
    elif range_str.endswith("d"):
        return int(range_str[:-1]) * 24
    return 24
