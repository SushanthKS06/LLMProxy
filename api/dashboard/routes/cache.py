# File: api/dashboard/routes/cache.py
# Cache performance endpoints

from fastapi import APIRouter, Query, Depends
from sqlalchemy.ext.asyncio import AsyncSession
from sqlalchemy import text
from datetime import datetime, timedelta

from database import get_db
from models.schemas import CacheStats, TopCachedPrompt

router = APIRouter(prefix="/api/v1/cache", tags=["cache"])


@router.get("/stats", response_model=CacheStats)
async def get_cache_stats(
    team_id: str = Query("default"),
    range: str = Query("24h"),
    db: AsyncSession = Depends(get_db),
):
    hours = parse_hours(range)
    since = datetime.utcnow() - timedelta(hours=hours)

    # Get total lookups
    total_result = await db.execute(
        text("""
            SELECT COUNT(*) as total
            FROM usage_log
            WHERE team_id = :team_id AND created_at >= :since
        """),
        {"team_id": team_id, "since": since}
    )
    total_lookups = total_result.scalar() or 0

    # Get hits
    hits_result = await db.execute(
        text("""
            SELECT COUNT(*) as hits
            FROM usage_log
            WHERE team_id = :team_id AND created_at >= :since AND cache_hit = true
        """),
        {"team_id": team_id, "since": since}
    )
    hits = hits_result.scalar() or 0

    # Get misses
    misses_result = await db.execute(
        text("""
            SELECT COUNT(*) as misses
            FROM usage_log
            WHERE team_id = :team_id AND created_at >= :since AND cache_hit = false
        """),
        {"team_id": team_id, "since": since}
    )
    misses = misses_result.scalar() or 0

    # Calculate hit rate
    hit_rate_pct = (hits / total_lookups * 100) if total_lookups > 0 else 0.0

    # Near misses would come from a separate counter/metric in production
    near_misses = 0

    # Get top cached prompts
    top_prompts_result = await db.execute(
        text("""
            SELECT 
                SUBSTRING(prompt_text, 1, 100) as prompt_preview,
                hit_count,
                hit_count * cost_usd as cost_saved
            FROM prompt_cache
            WHERE team_id = :team_id
            ORDER BY hit_count DESC
            LIMIT 10
        """),
        {"team_id": team_id}
    )
    top_cached_prompts = [
        TopCachedPrompt(
            prompt_preview=row[0] if row[0] else "",
            hit_count=row[1],
            cost_saved_usd=float(row[2]) if row[2] else 0.0,
        )
        for row in top_prompts_result.fetchall()
    ]

    # Get average similarity on hits (would need similarity tracking in DB)
    avg_similarity = 0.94  # Placeholder

    return CacheStats(
        hit_rate_pct=hit_rate_pct,
        total_lookups=total_lookups,
        hits=hits,
        misses=misses,
        near_misses=near_misses,
        top_cached_prompts=top_cached_prompts,
        avg_similarity_on_hits=avg_similarity,
    )


def parse_hours(range_str: str) -> int:
    """Parse range string like '24h', '7d' to hours."""
    range_str = range_str.lower().strip()
    if range_str.endswith("h"):
        return int(range_str[:-1])
    elif range_str.endswith("d"):
        return int(range_str[:-1]) * 24
    return 24  # Default to 24 hours
