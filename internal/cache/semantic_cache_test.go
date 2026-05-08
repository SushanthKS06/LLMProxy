// File: internal/cache/semantic_cache_test.go

package cache

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHashText(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "simple text",
			input:    "hello world",
			expected: "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := hashText(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGetPromptHash(t *testing.T) {
	hash1 := GetPromptHash("test prompt")
	hash2 := GetPromptHash("test prompt")
	hash3 := GetPromptHash("different prompt")

	// Same input should produce same hash
	assert.Equal(t, hash1, hash2)

	// Different input should produce different hash
	assert.NotEqual(t, hash1, hash3)
}
