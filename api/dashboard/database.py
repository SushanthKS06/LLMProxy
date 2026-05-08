# File: api/dashboard/database.py
# Async SQLAlchemy database connection

from sqlalchemy.ext.asyncio import create_async_engine, AsyncSession, async_sessionmaker
from sqlalchemy.orm import declarative_base
from sqlalchemy import text
from typing import AsyncGenerator

from config import get_settings

settings = get_settings()

# Convert postgres:// to postgresql+asyncpg://
dsn = settings.postgres_dsn
if dsn.startswith("postgres://"):
    dsn = dsn.replace("postgres://", "postgresql+asyncpg://", 1)

engine = create_async_engine(
    dsn,
    echo=False,
    pool_size=10,
    max_overflow=20,
    pool_pre_ping=True,
    pool_recycle=3600,
)

async_session_maker = async_sessionmaker(
    engine,
    class_=AsyncSession,
    expire_on_commit=False,
)

Base = declarative_base()


async def get_db() -> AsyncGenerator[AsyncSession, None]:
    async with async_session_maker() as session:
        try:
            yield session
        finally:
            await session.close()


async def check_db_health() -> bool:
    async with async_session_maker() as session:
        try:
            await session.execute(text("SELECT 1"))
            return True
        except Exception:
            return False
