// File: internal/db/redis.go
// WHY: Redis is used as a hot cache layer alongside pgvector for sub-5ms lookups.
// The 2-layer cache architecture: Redis stores exact prompt_hash lookups (<1ms),
// while pgvector handles semantic similarity search for cache misses.

package db

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/sushanthks/llm-gateway/internal/config"
)

// NewRedisClient creates a new Redis client.
func NewRedisClient(cfg *config.DatabaseConfig) *redis.Client {
	rdb := redis.NewClient(&redis.Options{
		Addr:         cfg.RedisAddr,
		Password:     cfg.RedisPassword,
		DB:           cfg.RedisDB,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
		PoolSize:     10,
		MinIdleConns: 2,
		MaxRetries:   3,
		RetryDelay:   100 * time.Millisecond,
	})
	return rdb
}

// HealthCheck verifies the Redis connection is healthy.
func HealthCheck(ctx context.Context, rdb *redis.Client) error {
	if rdb == nil {
		return fmt.Errorf("redis client is nil")
	}
	return rdb.Ping(ctx).Err()
}

// RedisClient wraps redis.Client for convenience methods.
type RedisClient struct {
	client *redis.Client
}

// NewRedis creates a new RedisClient wrapper.
func NewRedis(cfg *config.DatabaseConfig) *RedisClient {
	return &RedisClient{
		client: NewRedisClient(cfg),
	}
}

// Get returns the underlying redis client.
func (r *RedisClient) Get() *redis.Client {
	return r.client
}

// Close closes the Redis connection.
func (r *RedisClient) Close() error {
	return r.client.Close()
}

// Get retrieves a value from Redis by key.
func Get(ctx context.Context, rdb *redis.Client, key string) (string, error) {
	return rdb.Get(ctx, key).Result()
}

// Set sets a value in Redis with optional expiration.
func Set(ctx context.Context, rdb *redis.Client, key string, value interface{}, expiration time.Duration) error {
	return rdb.Set(ctx, key, value, expiration).Err()
}

// Del deletes keys from Redis.
func Del(ctx context.Context, rdb *redis.Client, keys ...string) error {
	return rdb.Del(ctx, keys...).Err()
}

// Exists checks if a key exists in Redis.
func Exists(ctx context.Context, rdb *redis.Client, key string) (bool, error) {
	n, err := rdb.Exists(ctx, key).Result()
	return n > 0, err
}

// TTL returns the remaining time to live for a key.
func TTL(ctx context.Context, rdb *redis.Client, key string) (time.Duration, error) {
	return rdb.TTL(ctx, key).Result()
}

// Incr increments a counter in Redis.
func Incr(ctx context.Context, rdb *redis.Client, key string) (int64, error) {
	return rdb.Incr(ctx, key).Result()
}

// HSet sets a field in a hash.
func HSet(ctx context.Context, rdb *redis.Client, key string, field string, value interface{}) error {
	return rdb.HSet(ctx, key, field, value).Err()
}

// HGet gets a field from a hash.
func HGet(ctx context.Context, rdb *redis.Client, key string, field string) (string, error) {
	return rdb.HGet(ctx, key, field).Result()
}

// HGetAll gets all fields from a hash.
func HGetAll(ctx context.Context, rdb *redis.Client, key string) (map[string]string, error) {
	return rdb.HGetAll(ctx, key).Result()
}

// SAdd adds members to a set.
func SAdd(ctx context.Context, rdb *redis.Client, key string, members ...interface{}) error {
	return rdb.SAdd(ctx, key, members...).Err()
}

// SMembers gets all members of a set.
func SMembers(ctx context.Context, rdb *redis.Client, key string) ([]string, error) {
	return rdb.SMembers(ctx, key).Result()
}

// Pipeline creates a new pipeline.
func Pipeline(ctx context.Context, rdb *redis.Client) redis.Pipeliner {
	return rdb.Pipeline()
}
