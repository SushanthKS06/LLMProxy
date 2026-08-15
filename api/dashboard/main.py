# File: api/dashboard/main.py
# FastAPI Dashboard Application

from contextlib import asynccontextmanager
from fastapi import FastAPI, Depends, HTTPException, Header
from fastapi.middleware.cors import CORSMiddleware

from config import get_settings
from database import engine, Base
from routes import costs, cache, models, latency, health


async def verify_api_key(x_dashboard_api_key: str = Header(None)):
    settings = get_settings()
    if not settings.dashboard_api_keys:
        return # Open mode

    valid_keys = [k.strip() for k in settings.dashboard_api_keys.split(",") if k.strip()]
    if not valid_keys:
        return
        
    if not x_dashboard_api_key or x_dashboard_api_key not in valid_keys:
        raise HTTPException(status_code=401, detail="Invalid API Key")

@asynccontextmanager
async def lifespan(app: FastAPI):
    # Startup: Create tables
    async with engine.begin() as conn:
        await conn.run_sync(Base.metadata.create_all)
    yield
    # Shutdown: Close engine
    await engine.dispose()


app = FastAPI(
    title="LLM Cost Intelligence Gateway - Dashboard",
    description="API for viewing gateway metrics, costs, and performance",
    version="1.0.0",
    lifespan=lifespan,
)

# CORS middleware
app.add_middleware(
    CORSMiddleware,
    allow_origins=["*"],
    allow_credentials=True,
    allow_methods=["*"],
    allow_headers=["*"],
)

# Register routes (apply auth dependency to all except health)
app.include_router(health.router)
app.include_router(costs.router, dependencies=[Depends(verify_api_key)])
app.include_router(cache.router, dependencies=[Depends(verify_api_key)])
app.include_router(models.router, dependencies=[Depends(verify_api_key)])
app.include_router(latency.router, dependencies=[Depends(verify_api_key)])


@app.get("/")
async def root():
    return {
        "service": "LLM Cost Intelligence Gateway - Dashboard",
        "version": "1.0.0",
        "docs": "/docs",
    }


if __name__ == "__main__":
    import uvicorn
    settings = get_settings()
    uvicorn.run(
        "main:app",
        host=settings.host,
        port=settings.port,
        log_level=settings.log_level.lower(),
        reload=False,
    )
