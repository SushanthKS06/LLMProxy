// File: internal/config/config_test.go

package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadConfig_Defaults(t *testing.T) {
	// Save original env
	origEnv := os.environ
	os.environ = &mockEnv{}
	defer func() { os.environ = origEnv }()

	// Clear singleton for test
	cfg = nil
	once = sync.Once{}

	// Set required env vars
	os.Setenv("OPENAI_API_KEY", "sk-test")
	os.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	os.Setenv("POSTGRES_DSN", "postgres://test:test@localhost:5432/testdb")
	os.Setenv("REDIS_ADDR", "localhost:6379")
	defer os.Unsetenv("OPENAI_API_KEY")
	defer os.Unsetenv("ANTHROPIC_API_KEY")
	defer os.Unsetenv("POSTGRES_DSN")
	defer os.Unsetenv("REDIS_ADDR")

	// Reset sync.Once
	once = sync.Once{}

	cfg = Load()

	require.NotNil(t, cfg, "config should not be nil")

	// Verify defaults
	assert.Equal(t, 8080, cfg.Server.Port, "default port should be 8080")
	assert.Equal(t, "text-embedding-3-small", cfg.Embeddings.EmbeddingModel, "default embedding model")
	assert.Equal(t, 0.92, cfg.Routing.SimilarityThreshold, "default similarity threshold")
	assert.Equal(t, 50, cfg.Routing.SimpleTokenThreshold, "default simple token threshold")
	assert.Equal(t, 300, cfg.Routing.ComplexTokenThreshold, "default complex token threshold")
	assert.Equal(t, 9090, cfg.Observability.PrometheusPort, "default prometheus port")
	assert.Equal(t, "info", cfg.Observability.LogLevel, "default log level")
	assert.Equal(t, "json", cfg.Observability.LogFormat, "default log format")
}

func TestLoadConfig_OverrideViaEnv(t *testing.T) {
	// Save original env
	origEnv := os.environ
	os.environ = &mockEnv{}
	defer func() { os.environ = origEnv }()

	// Set required env vars with overrides
	os.Setenv("OPENAI_API_KEY", "sk-test")
	os.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	os.Setenv("POSTGRES_DSN", "postgres://test:test@localhost:5432/testdb")
	os.Setenv("REDIS_ADDR", "localhost:6379")
	os.Setenv("GATEWAY_PORT", "9090")
	os.Setenv("SIMILARITY_THRESHOLD", "0.95")
	os.Setenv("LOG_LEVEL", "debug")
	defer os.Unsetenv("OPENAI_API_KEY")
	defer os.Unsetenv("ANTHROPIC_API_KEY")
	defer os.Unsetenv("POSTGRES_DSN")
	defer os.Unsetenv("REDIS_ADDR")
	defer os.Unsetenv("GATEWAY_PORT")
	defer os.Unsetenv("SIMILARITY_THRESHOLD")
	defer os.Unsetenv("LOG_LEVEL")

	// Reset singleton
	cfg = nil
	once = sync.Once{}

	cfg = Load()

	require.NotNil(t, cfg)
	assert.Equal(t, 9090, cfg.Server.Port, "port should be overridden")
	assert.Equal(t, 0.95, cfg.Routing.SimilarityThreshold, "similarity threshold should be overridden")
	assert.Equal(t, "debug", cfg.Observability.LogLevel, "log level should be overridden")
}

func TestLoadConfig_PanicOnMissingRequiredField(t *testing.T) {
	tests := []struct {
		name    string
		envVars map[string]string
		missing string
	}{
		{
			name:    "missing OPENAI_API_KEY",
			envVars: map[string]string{},
			missing: "OPENAI_API_KEY",
		},
		{
			name: "missing ANTHROPIC_API_KEY",
			envVars: map[string]string{
				"OPENAI_API_KEY": "sk-test",
			},
			missing: "ANTHROPIC_API_KEY",
		},
		{
			name: "missing POSTGRES_DSN",
			envVars: map[string]string{
				"OPENAI_API_KEY":  "sk-test",
				"ANTHROPIC_API_KEY": "sk-ant-test",
			},
			missing: "POSTGRES_DSN",
		},
		{
			name: "missing REDIS_ADDR",
			envVars: map[string]string{
				"OPENAI_API_KEY":  "sk-test",
				"ANTHROPIC_API_KEY": "sk-ant-test",
				"POSTGRES_DSN":    "postgres://test:test@localhost:5432/testdb",
			},
			missing: "REDIS_ADDR",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clear env
			os.Unsetenv("OPENAI_API_KEY")
			os.Unsetenv("ANTHROPIC_API_KEY")
			os.Unsetenv("POSTGRES_DSN")
			os.Unsetenv("REDIS_ADDR")

			// Set test env vars
			for k, v := range tt.envVars {
				os.Setenv(k, v)
			}
			defer func() {
				for k := range tt.envVars {
					os.Unsetenv(k)
				}
			}()

			// Reset singleton
			cfg = nil
			once = sync.Once{}

			// Should panic
			assert.Panics(t, func() {
				Load()
			}, "should panic when %s is missing", tt.missing)
		})
	}
}

func TestGetAPIKeys(t *testing.T) {
	tests := []struct {
		name     string
		envValue string
		expected []string
	}{
		{
			name:     "empty",
			envValue: "",
			expected: nil,
		},
		{
			name:     "single key",
			envValue: "key1",
			expected: []string{"key1"},
		},
		{
			name:     "multiple keys",
			envValue: "key1,key2,key3",
			expected: []string{"key1", "key2", "key3"},
		},
		{
			name:     "keys with spaces",
			envValue: "key1, key2 , key3",
			expected: []string{"key1", "key2", "key3"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envValue == "" {
				os.Unsetenv("GATEWAY_API_KEYS")
			} else {
				os.Setenv("GATEWAY_API_KEYS", tt.envValue)
				defer os.Unsetenv("GATEWAY_API_KEYS")
			}

			result := GetAPIKeys()
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGetRateLimitConfig(t *testing.T) {
	tests := []struct {
		name      string
		rps       string
		burst     string
		wantRps   int
		wantBurst int
		wantErr   bool
	}{
		{
			name:      "valid defaults",
			rps:       "",
			burst:     "",
			wantRps:   100,
			wantBurst: 20,
			wantErr:   false,
		},
		{
			name:      "valid custom values",
			rps:       "200",
			burst:     "50",
			wantRps:   200,
			wantBurst: 50,
			wantErr:   false,
		},
		{
			name:    "invalid rps",
			rps:     "0",
			burst:   "20",
			wantErr: true,
		},
		{
			name:    "invalid burst",
			rps:     "100",
			burst:   "-1",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.rps == "" {
				os.Unsetenv("GATEWAY_RATE_LIMIT_RPS")
			} else {
				os.Setenv("GATEWAY_RATE_LIMIT_RPS", tt.rps)
				defer os.Unsetenv("GATEWAY_RATE_LIMIT_RPS")
			}
			if tt.burst == "" {
				os.Unsetenv("GATEWAY_RATE_LIMIT_BURST")
			} else {
				os.Setenv("GATEWAY_RATE_LIMIT_BURST", tt.burst)
				defer os.Unsetenv("GATEWAY_RATE_LIMIT_BURST")
			}

			rps, burst, err := GetRateLimitConfig()
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.wantRps, rps)
				assert.Equal(t, tt.wantBurst, burst)
			}
		})
	}
}

// mockEnv implements os environment interface for testing
type mockEnv struct{}

func (m *mockEnv) Getenv(key string) string {
	return os.Getenv(key)
}

func (m *mockEnv) Setenv(key, value string) error {
	return os.Setenv(key, value)
}

func (m *mockEnv) Unsetenv(key string) error {
	return os.Unsetenv(key)
}

func (m *mockEnv) Environ() []string {
	return os.Environ()
}
