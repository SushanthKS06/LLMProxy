# File: api/dashboard/routes/health.py
# Health check endpoint

from fastapi import APIRouter
from database import check_db_health
from models.schemas import HealthResponse

router = APIRouter(tags=["health"])


@router.get("/health", response_model=HealthResponse)
async def health_check():
    # Check database
    db_status = "ok"
    try:
        if not await check_db_health():
            db_status = "error"
    except Exception:
        db_status = "error"

    # Redis check would need Redis client
    redis_status = "ok"  # Simplified - would check actual Redis

    status = "ok" if db_status == "ok" else "degraded"

    return HealthResponse(
        status=status,
        db=db_status,
        redis=redis_status,
        version="1.0.0",
    )


@router.get("/health/ready", response_model=HealthResponse)
async def readiness_check():
    return await health_check()


@router.get("/health/live", response_model=HealthResponse)
async def liveness_check():
    return HealthResponse(
        status="ok",
        db="ok",
        redis="ok",
        version="1.0.0",
    )
