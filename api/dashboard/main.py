# File: api/dashboard/main.py
# FastAPI Dashboard Application

from contextlib import asynccontextmanager
from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware

from config import get_settings
from database import engine, Base
from routes import costs, cache, models, latency, health


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

# Register routes
app.include_router(health.router)
app.include_router(costs.router)
app.include_router(cache.router)
app.include_router(models.router)
app.include_router(latency.router)


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
