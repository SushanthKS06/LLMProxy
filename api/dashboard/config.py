# File: api/dashboard/config.py
# Dashboard configuration using Pydantic settings

from pydantic_settings import BaseSettings
from functools import lru_cache


class Settings(BaseSettings):
    # Database
    postgres_dsn: str = "postgres://gateway:gateway@localhost:5432/llmgateway"
    
    # Server
    host: str = "0.0.0.0"
    port: int = 8000
    
    # Observability
    log_level: str = "info"
    
    class Config:
        env_file = ".env"
        env_file_encoding = "utf-8"
        extra = "allow"


@lru_cache()
def get_settings() -> Settings:
    return Settings()
