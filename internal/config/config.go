// File: internal/config/config.go
// WHY: Centralized configuration management with environment variable loading,
// validation, and sensible defaults following 12-factor app principles.

package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
)

// Config holds all configuration for the gateway.
// It is immutable after loading - no setters provided.
type Config struct {
	Server        ServerConfig
	Database      DatabaseConfig
	Embeddings    EmbeddingsConfig
	Routing       RoutingConfig
	Providers     ProvidersConfig
	Cost          map[string]ModelCost
	Observability ObservabilityConfig
	Redaction     RedactionConfig
}

// ServerConfig holds HTTP server configuration.
type ServerConfig struct {
	Port                  int
	ReadTimeout           int
	WriteTimeout          int
	MaxConcurrentRequests int
}

// DatabaseConfig holds database connection configuration.
type DatabaseConfig struct {
	PostgresDSN   string
	RedisAddr     string
	RedisPassword string
	RedisDB       int
	MaxConns      int32
	MinConns      int32
}

// EmbeddingsConfig holds embedding service configuration.
type EmbeddingsConfig struct {
	OpenAIAPIKey       string
	EmbeddingBaseURL   string
	EmbeddingModel     string
	EmbeddingBatchSize int
	BatchTimeoutMs     int
}

// RoutingConfig holds model routing configuration.
type RoutingConfig struct {
	DefaultModel          string
	SimilarityThreshold   float64
	SimpleTokenThreshold  int
	ComplexTokenThreshold int
}

// ProvidersConfig holds LLM provider API configuration.
type ProvidersConfig struct {
	OpenAIBaseURL    string
	AnthropicBaseURL string
	AnthropicAPIKey  string
	OpenAIAPIKey     string
	GroqBaseURL      string
	GroqAPIKey       string
}

// ModelCost holds pricing information for a model.
type ModelCost struct {
	InputPer1M  float64 // USD per 1M input tokens
	OutputPer1M float64 // USD per 1M output tokens
}

// ObservabilityConfig holds logging and metrics configuration.
type ObservabilityConfig struct {
	PrometheusPort int
	LogLevel       string
	LogFormat      string
}

// RedactionConfig holds PII redaction settings.
type RedactionConfig struct {
	// Enabled is a master switch; set REDACTION_ENABLED=false to bypass entirely.
	Enabled bool
	// FailClosed controls whether redaction failures block requests (REDACTION_FAIL_CLOSED=true)
	// or pass through unredacted (default: false = fail open).
	FailClosed bool
	// DisabledPatterns is a list of pattern names to skip (REDACTION_DISABLED_PATTERNS=ipv4,phone_us).
	DisabledPatterns []string
}

var (
	cfg  *Config
	once sync.Once
)

// Load loads configuration from environment variables with defaults.
// Panics if required fields are missing.
func Load() *Config {
	once.Do(func() {
		cfg = loadConfig()
	})
	return cfg
}

// Reset clears the config singleton so that Load() can be called again.
// THIS FUNCTION IS INTENDED FOR USE IN TESTS ONLY.
// Calling it in production code is a bug — the singleton exists to guarantee
// a single consistent config for the lifetime of the process.
//
//nolint:unused // used in test packages
func Reset() {
	once = sync.Once{}
	cfg = nil
}

// loadConfig performs the actual configuration loading.
func loadConfig() *Config {
	// Required fields - panic if missing
	openAIKey := getEnv("OPENAI_API_KEY", "")
	anthropicKey := getEnv("ANTHROPIC_API_KEY", "")
	groqKey := getEnv("GROQ_API_KEY", "")
	postgresDSN := getEnv("POSTGRES_DSN", "")
	redisAddr := getEnv("REDIS_ADDR", "")

	if groqKey == "" {
		panic("GROQ_API_KEY is required")
	}
	if postgresDSN == "" {
		panic("POSTGRES_DSN is required")
	}
	if redisAddr == "" {
		panic("REDIS_ADDR is required")
	}

	// Default model costs (USD per 1M tokens)
	costMap := map[string]ModelCost{
		"openai/gpt-4o-mini":        {InputPer1M: 0.15, OutputPer1M: 0.60},
		"openai/gpt-4o":             {InputPer1M: 2.50, OutputPer1M: 10.00},
		// Anthropic Models
		"anthropic/claude-haiku-3":  {InputPer1M: 0.25, OutputPer1M: 1.25},
		"anthropic/claude-sonnet-4": {InputPer1M: 3.00, OutputPer1M: 15.00},

		// Groq Models
		"groq/gpt-oss-20b":          {InputPer1M: 0.075, OutputPer1M: 0.30},
		"groq/llama-3.3-70b-versatile": {InputPer1M: 0.59, OutputPer1M: 0.79},
		"groq/gpt-oss-120b":         {InputPer1M: 0.15, OutputPer1M: 0.60},
	}

	cfg := &Config{
		Server: ServerConfig{
			Port:                  getEnvInt("GATEWAY_PORT", 8080),
			ReadTimeout:           getEnvInt("GATEWAY_READ_TIMEOUT", 30),
			WriteTimeout:          getEnvInt("GATEWAY_WRITE_TIMEOUT", 120),
			MaxConcurrentRequests: getEnvInt("GATEWAY_MAX_CONCURRENT", 500),
		},
		Database: DatabaseConfig{
			PostgresDSN:   postgresDSN,
			RedisAddr:     redisAddr,
			RedisPassword: getEnv("REDIS_PASSWORD", ""),
			RedisDB:       getEnvInt("REDIS_DB", 0),
			MaxConns:      getEnvInt32("POSTGRES_MAX_CONNS", 20),
			MinConns:      getEnvInt32("POSTGRES_MIN_CONNS", 2),
		},
		Embeddings: EmbeddingsConfig{
			OpenAIAPIKey:       openAIKey,
			EmbeddingBaseURL:   getEnv("EMBEDDING_BASE_URL", ""),
			EmbeddingModel:     getEnv("EMBEDDING_MODEL", "text-embedding-3-small"),
			EmbeddingBatchSize: getEnvInt("EMBEDDING_BATCH_SIZE", 20),
			BatchTimeoutMs:     getEnvInt("EMBEDDING_BATCH_TIMEOUT_MS", 50),
		},
		Routing: RoutingConfig{
			DefaultModel:          getEnv("DEFAULT_MODEL", "anthropic/claude-haiku-3"),
			SimilarityThreshold:   getEnvFloat("SIMILARITY_THRESHOLD", 0.92),
			SimpleTokenThreshold:  getEnvInt("SIMPLE_TOKEN_THRESHOLD", 50),
			ComplexTokenThreshold: getEnvInt("COMPLEX_TOKEN_THRESHOLD", 300),
		},
		Providers: ProvidersConfig{
			OpenAIBaseURL:    getEnv("OPENAI_BASE_URL", "https://api.openai.com/v1"),
			AnthropicBaseURL: getEnv("ANTHROPIC_BASE_URL", "https://api.anthropic.com"),
			AnthropicAPIKey:  anthropicKey,
			OpenAIAPIKey:     openAIKey,
			GroqBaseURL:      getEnv("GROQ_BASE_URL", "https://api.groq.com/openai/v1"),
			GroqAPIKey:       groqKey,
		},
		Cost: costMap,
		Observability: ObservabilityConfig{
			PrometheusPort: getEnvInt("PROMETHEUS_PORT", 9090),
			LogLevel:       getEnv("LOG_LEVEL", "info"),
			LogFormat:      getEnv("LOG_FORMAT", "json"),
		},
		Redaction: RedactionConfig{
			Enabled:          getEnvBool("REDACTION_ENABLED", true),
			FailClosed:       getEnvBool("REDACTION_FAIL_CLOSED", false),
			DisabledPatterns: getEnvStringSlice("REDACTION_DISABLED_PATTERNS", nil),
		},
	}

	return cfg
}

// Get returns the loaded config (panics if not loaded).
func Get() *Config {
	if cfg == nil {
		panic("config not loaded - call Load() first")
	}
	return cfg
}

// GetEnv retrieves an environment variable or returns default.
func getEnv(key, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
}

// GetEnvInt retrieves an environment variable as int or returns default.
func getEnvInt(key string, defaultValue int) int {
	if value, exists := os.LookupEnv(key); exists {
		if intVal, err := strconv.Atoi(value); err == nil {
			return intVal
		}
	}
	return defaultValue
}

// GetEnvInt32 retrieves an environment variable as int32 or returns default.
func getEnvInt32(key string, defaultValue int32) int32 {
	if value, exists := os.LookupEnv(key); exists {
		if intVal, err := strconv.Atoi(value); err == nil {
			return int32(intVal)
		}
	}
	return defaultValue
}

// getEnvBool retrieves an environment variable as bool or returns default.
// Accepts "true", "1", "yes" (case-insensitive) as true; everything else is false.
func getEnvBool(key string, defaultValue bool) bool {
	if value, exists := os.LookupEnv(key); exists {
		v := strings.ToLower(strings.TrimSpace(value))
		return v == "true" || v == "1" || v == "yes"
	}
	return defaultValue
}

// getEnvStringSlice retrieves a comma-separated env var as a string slice.
func getEnvStringSlice(key string, defaultValue []string) []string {
	if value, exists := os.LookupEnv(key); exists && value != "" {
		parts := strings.Split(value, ",")
		result := make([]string, 0, len(parts))
		for _, p := range parts {
			if t := strings.TrimSpace(p); t != "" {
				result = append(result, t)
			}
		}
		return result
	}
	return defaultValue
}

// GetEnvFloat retrieves an environment variable as float64 or returns default.
func getEnvFloat(key string, defaultValue float64) float64 {
	if value, exists := os.LookupEnv(key); exists {
		if floatVal, err := strconv.ParseFloat(value, 64); err == nil {
			return floatVal
		}
	}
	return defaultValue
}

// GetAPIKeys returns the list of valid API keys from environment.
func GetAPIKeys() []string {
	keysStr := getEnv("GATEWAY_API_KEYS", "")
	if keysStr == "" {
		return nil
	}
	var keys []string
	for _, k := range strings.Split(keysStr, ",") {
		k = strings.TrimSpace(k)
		if k != "" {
			keys = append(keys, k)
		}
	}
	return keys
}

// GetRateLimitConfig returns rate limiting configuration.
func GetRateLimitConfig() (rps int, burst int, err error) {
	rps = getEnvInt("GATEWAY_RATE_LIMIT_RPS", 100)
	burst = getEnvInt("GATEWAY_RATE_LIMIT_BURST", 20)
	if rps <= 0 {
		return 0, 0, fmt.Errorf("GATEWAY_RATE_LIMIT_RPS must be positive, got %d", rps)
	}
	if burst <= 0 {
		return 0, 0, fmt.Errorf("GATEWAY_RATE_LIMIT_BURST must be positive, got %d", burst)
	}
	return rps, burst, nil
}

// GetAllowedOrigins returns the list of allowed CORS origins from environment.
// FIX (AUDIT-J): Wires GATEWAY_ALLOWED_ORIGINS to CORSMiddleware so production
// deployments can restrict cross-origin access to known frontend domains.
// Defaults to ["*"] (open) for local development; always set this in production.
func GetAllowedOrigins() []string {
	originsStr := getEnv("GATEWAY_ALLOWED_ORIGINS", "")
	if originsStr == "" {
		return []string{"*"}
	}
	var origins []string
	for _, o := range strings.Split(originsStr, ",") {
		o = strings.TrimSpace(o)
		if o != "" {
			origins = append(origins, o)
		}
	}
	if len(origins) == 0 {
		return []string{"*"}
	}
	return origins
}
