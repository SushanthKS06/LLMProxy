# File: api/dashboard/models/schemas.py
# Pydantic response schemas for the dashboard API

from pydantic import BaseModel
from typing import Optional, List, Dict
from datetime import datetime


# Cost endpoints
class CostByDay(BaseModel):
    date: str
    cost: float


class CostByModel(BaseModel):
    model: str
    cost_usd: float
    request_count: int
    avg_cost_per_request: float


class CostByFeature(BaseModel):
    feature: str
    cost_usd: float
    request_count: int


class CostSummary(BaseModel):
    total_cost_usd: float
    cost_by_model: Dict[str, float]
    cost_by_feature: Dict[str, float]
    cost_by_day: List[CostByDay]
    total_cost_saved_usd: float
    projected_monthly_cost: float


class CostBreakdownItem(BaseModel):
    label: str
    cost_usd: float
    request_count: int
    avg_cost_per_request: float


# Cache endpoints
class TopCachedPrompt(BaseModel):
    prompt_preview: str
    hit_count: int
    cost_saved_usd: float


class CacheStats(BaseModel):
    hit_rate_pct: float
    total_lookups: int
    hits: int
    misses: int
    near_misses: int
    top_cached_prompts: List[TopCachedPrompt]
    avg_similarity_on_hits: float


# Model distribution endpoints
class ModelUsage(BaseModel):
    model: str
    count: int
    total_cost: float
    avg_latency_ms: float


class ModelDistribution(BaseModel):
    by_complexity: Dict[str, float]
    by_model: List[ModelUsage]


# Latency endpoints
class LatencyPercentiles(BaseModel):
    p50_ms: float
    p95_ms: float
    p99_ms: float
    by_model: Dict[str, Dict[str, float]]
    by_cache_hit: Dict[str, float]


# Health endpoint
class HealthResponse(BaseModel):
    status: str
    db: str
    redis: str
    version: str = "1.0.0"
