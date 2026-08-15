# File: api/dashboard/routes/costs.py
# Cost analytics endpoints

from fastapi import APIRouter, Query, Depends
from sqlalchemy.ext.asyncio import AsyncSession
from sqlalchemy import text
from datetime import datetime, timedelta
from typing import Optional

from ..database import get_db
from ..models.schemas import CostSummary, CostByDay, CostBreakdownItem

router = APIRouter(prefix="/api/v1/costs", tags=["costs"])


@router.get("/summary", response_model=CostSummary)
async def get_cost_summary(
    team_id: str = Query("default"),
    range: str = Query("7d"),
    db: AsyncSession = Depends(get_db),
):
    days = parse_range(range)
    since = datetime.utcnow() - timedelta(days=days)

    # Get total cost
    total_cost_result = await db.execute(
        text("""
            SELECT COALESCE(SUM(cost_usd), 0) as total_cost
            FROM usage_log
            WHERE team_id = :team_id AND created_at >= :since
        """),
        {"team_id": team_id, "since": since}
    )
    total_cost = total_cost_result.scalar() or 0.0

    # Get cost by model
    cost_by_model_result = await db.execute(
        text("""
            SELECT model_used, COALESCE(SUM(cost_usd), 0) as cost
            FROM usage_log
            WHERE team_id = :team_id AND created_at >= :since
            GROUP BY model_used
        """),
        {"team_id": team_id, "since": since}
    )
    cost_by_model = {row[0]: float(row[1]) for row in cost_by_model_result.fetchall()}

    # Get cost by feature
    cost_by_feature_result = await db.execute(
        text("""
            SELECT feature_tag, COALESCE(SUM(cost_usd), 0) as cost
            FROM usage_log
            WHERE team_id = :team_id AND created_at >= :since
            GROUP BY feature_tag
        """),
        {"team_id": team_id, "since": since}
    )
    cost_by_feature = {row[0]: float(row[1]) for row in cost_by_feature_result.fetchall()}

    # Get cost by day using TimescaleDB time_bucket
    cost_by_day_result = await db.execute(
        text("""
            SELECT time_bucket('1 day', created_at) as day, COALESCE(SUM(cost_usd), 0) as cost
            FROM usage_log
            WHERE team_id = :team_id AND created_at >= :since
            GROUP BY day
            ORDER BY day
        """),
        {"team_id": team_id, "since": since}
    )
    cost_by_day = [
        CostByDay(date=row[0].strftime("%Y-%m-%d"), cost=float(row[1]))
        for row in cost_by_day_result.fetchall()
    ]

    # Get savings from cache
    cache_savings_result = await db.execute(
        text("""
            SELECT COALESCE(SUM(cost_saved_usd), 0) as savings
            FROM usage_log
            WHERE team_id = :team_id AND created_at >= :since
        """),
        {"team_id": team_id, "since": since}
    )
    savings = float(cache_savings_result.scalar() or 0.0)

    # Project monthly cost
    daily_avg = total_cost / days if days > 0 else 0
    projected_monthly = daily_avg * 30

    return CostSummary(
        total_cost_usd=total_cost,
        cost_by_model=cost_by_model,
        cost_by_feature=cost_by_feature,
        cost_by_day=cost_by_day,
        total_cost_saved_usd=savings,
        projected_monthly_cost=projected_monthly,
    )


@router.get("/breakdown", response_model=list[CostBreakdownItem])
async def get_cost_breakdown(
    team_id: str = Query("default"),
    range: str = Query("7d"),
    group_by: str = Query("model"),
    db: AsyncSession = Depends(get_db),
):
    days = parse_range(range)
    since = datetime.utcnow() - timedelta(days=days)

    if group_by == "model":
        query = text("""
            SELECT 
                model_used as label,
                COALESCE(SUM(cost_usd), 0) as cost_usd,
                COUNT(*) as request_count,
                COALESCE(AVG(cost_usd), 0) as avg_cost
            FROM usage_log
            WHERE team_id = :team_id AND created_at >= :since
            GROUP BY model_used
            ORDER BY cost_usd DESC
        """)
    elif group_by == "feature":
        query = text("""
            SELECT 
                feature_tag as label,
                COALESCE(SUM(cost_usd), 0) as cost_usd,
                COUNT(*) as request_count,
                COALESCE(AVG(cost_usd), 0) as avg_cost
            FROM usage_log
            WHERE team_id = :team_id AND created_at >= :since
            GROUP BY feature_tag
            ORDER BY cost_usd DESC
        """)
    elif group_by == "day":
        query = text("""
            SELECT 
                time_bucket('1 day', created_at)::date as label,
                COALESCE(SUM(cost_usd), 0) as cost_usd,
                COUNT(*) as request_count,
                COALESCE(AVG(cost_usd), 0) as avg_cost
            FROM usage_log
            WHERE team_id = :team_id AND created_at >= :since
            GROUP BY label
            ORDER BY label DESC
        """)
    else:
        return []

    result = await db.execute(query, {"team_id": team_id, "since": since})
    return [
        CostBreakdownItem(
            label=str(row[0]),
            cost_usd=float(row[1]),
            request_count=row[2],
            avg_cost_per_request=float(row[3]),
        )
        for row in result.fetchall()
    ]


def parse_range(range_str: str) -> int:
    """Parse range string like '7d', '24h', '30d' to days."""
    range_str = range_str.lower().strip()
    if range_str.endswith("d"):
        return int(range_str[:-1])
    elif range_str.endswith("h"):
        return int(range_str[:-1]) // 24
    elif range_str.endswith("m"):
        return int(range_str[:-1]) * 30
    return 7  # Default to 7 days
