// File: internal/config/config_test.go

package config

import (
	"os"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resetSingleton clears the package-level singleton so each test gets a fresh Load().
// WHY: config uses sync.Once, so without resetting it every test would re-use
// the first call's result.  This is safe in tests since they run sequentially
// within the package (go test -count=1).
func resetSingleton() {
	cfg = nil
	once = sync.Once{}
}

// setRequiredEnv sets the four mandatory env vars and returns a cleanup func.
func setRequiredEnv(t *testing.T, extra ...string) {
	t.Helper()
	required := []string{
		"OPENAI_API_KEY", "sk-test",
		"ANTHROPIC_API_KEY", "sk-ant-test",
		"GROQ_API_KEY", "sk-groq-test",
		"POSTGRES_DSN", "postgres://test:test@localhost:5432/testdb",
		"REDIS_ADDR", "localhost:6379",
	}
	all := append(required, extra...)
	for i := 0; i < len(all)-1; i += 2 {
		os.Setenv(all[i], all[i+1])
	}
	t.Cleanup(func() {
		for i := 0; i < len(all)-1; i += 2 {
			os.Unsetenv(all[i])
		}
	})
}

func TestLoadConfig_Defaults(t *testing.T) {
	resetSingleton()
	setRequiredEnv(t)

	cfg = Load()

	require.NotNil(t, cfg, "config should not be nil")
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
	resetSingleton()
	setRequiredEnv(t,
		"GATEWAY_PORT", "9090",
		"SIMILARITY_THRESHOLD", "0.95",
		"LOG_LEVEL", "debug",
	)

	cfg = Load()

	require.NotNil(t, cfg)
	assert.Equal(t, 9090, cfg.Server.Port, "port should be overridden")
	assert.Equal(t, 0.95, cfg.Routing.SimilarityThreshold, "similarity threshold should be overridden")
	assert.Equal(t, "debug", cfg.Observability.LogLevel, "log level should be overridden")
}

func TestLoadConfig_PanicOnMissingRequiredField(t *testing.T) {
	// FIX (AUDIT-B): config.go panics ONLY on missing GROQ_API_KEY, POSTGRES_DSN,
	// and REDIS_ADDR. OPENAI_API_KEY and ANTHROPIC_API_KEY are optional — they are
	// used for embeddings and provider fallback but the gateway routes via Groq by
	// default. The previous test table incorrectly claimed panics for those keys.
	tests := []struct {
		name    string
		envVars map[string]string
		missing string
	}{
		{
			name:    "missing GROQ_API_KEY",
			envVars: map[string]string{},
			missing: "GROQ_API_KEY",
		},
		{
			name: "missing POSTGRES_DSN",
			envVars: map[string]string{
				"GROQ_API_KEY": "sk-groq-test",
			},
			missing: "POSTGRES_DSN",
		},
		{
			name: "missing REDIS_ADDR",
			envVars: map[string]string{
				"GROQ_API_KEY": "sk-groq-test",
				"POSTGRES_DSN": "postgres://test:test@localhost:5432/testdb",
			},
			missing: "REDIS_ADDR",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clear all required env vars
			os.Unsetenv("OPENAI_API_KEY")
			os.Unsetenv("ANTHROPIC_API_KEY")
			os.Unsetenv("GROQ_API_KEY")
			os.Unsetenv("POSTGRES_DSN")
			os.Unsetenv("REDIS_ADDR")

			// Set only the ones in the test case
			for k, v := range tt.envVars {
				os.Setenv(k, v)
			}
			defer func() {
				for k := range tt.envVars {
					os.Unsetenv(k)
				}
			}()

			// Reset singleton so Load() actually runs
			resetSingleton()

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
