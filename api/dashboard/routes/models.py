# File: api/dashboard/routes/models.py
# Model usage distribution endpoints

from fastapi import APIRouter, Query, Depends
from sqlalchemy.ext.asyncio import AsyncSession
from sqlalchemy import text
from datetime import datetime, timedelta

from database import get_db
from models.schemas import ModelDistribution, ModelUsage

router = APIRouter(prefix="/api/v1/models", tags=["models"])


@router.get("/distribution", response_model=ModelDistribution)
async def get_model_distribution(
    team_id: str = Query("default"),
    range: str = Query("7d"),
    db: AsyncSession = Depends(get_db),
):
    days = parse_range(range)
    since = datetime.utcnow() - timedelta(days=days)

    # Get total requests by complexity
    total_result = await db.execute(
        text("""
            SELECT COUNT(*) as total
            FROM usage_log
            WHERE team_id = :team_id AND created_at >= :since
        """),
        {"team_id": team_id, "since": since}
    )
    total = total_result.scalar() or 1  # Avoid division by zero

    # Get complexity distribution
    complexity_result = await db.execute(
        text("""
            SELECT complexity, COUNT(*) as count
            FROM usage_log
            WHERE team_id = :team_id AND created_at >= :since
            GROUP BY complexity
        """),
        {"team_id": team_id, "since": since}
    )

    by_complexity = {}
    for row in complexity_result.fetchall():
        complexity = row[0] or "medium"
        count = row[1]
        by_complexity[complexity] = (count / total * 100)

    # Ensure all complexities are present
    for c in ["simple", "medium", "complex"]:
        if c not in by_complexity:
            by_complexity[c] = 0.0

    # Get model usage
    model_result = await db.execute(
        text("""
            SELECT 
                model_used,
                COUNT(*) as count,
                COALESCE(SUM(cost_usd), 0) as total_cost,
                COALESCE(AVG(latency_ms), 0) as avg_latency
            FROM usage_log
            WHERE team_id = :team_id AND created_at >= :since
            GROUP BY model_used
            ORDER BY count DESC
        """),
        {"team_id": team_id, "since": since}
    )

    by_model = [
        ModelUsage(
            model=row[0],
            count=row[1],
            total_cost=float(row[2]),
            avg_latency_ms=float(row[3]),
        )
        for row in model_result.fetchall()
    ]

    return ModelDistribution(
        by_complexity=by_complexity,
        by_model=by_model,
    )


def parse_range(range_str: str) -> int:
    """Parse range string to days."""
    range_str = range_str.lower().strip()
    if range_str.endswith("d"):
        return int(range_str[:-1])
    elif range_str.endswith("h"):
        return int(range_str[:-1]) // 24
    return 7
