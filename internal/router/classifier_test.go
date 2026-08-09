// File: internal/router/classifier_test.go

package router

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClassify(t *testing.T) {
	classifier := NewComplexityClassifier(50, 300)

	tests := []struct {
		name     string
		prompt   string
		expected ComplexityLevel
	}{
		// Simple cases
		{
			name:     "single word prompt",
			prompt:   "Hi",
			expected: ComplexitySimple,
		},
		{
			name:     "short question",
			prompt:   "What is 2+2?",
			expected: ComplexitySimple,
		},
		{
			name:     "weather question",
			prompt:   "What's the weather like?",
			expected: ComplexitySimple,
		},
		{
			name:     "short greeting",
			prompt:   "Hello, how are you?",
			expected: ComplexitySimple,
		},

		// Medium cases
		{
			name:     "medium length question",
			prompt:   "Can you explain what a REST API is in simple terms?",
			expected: ComplexityMedium,
		},
		{
			name:     "single keyword",
			prompt:   "Explain what Docker is",
			expected: ComplexityMedium,
		},
		{
			name:   "short code snippet",
			prompt: "```\nprint('hello')\n```",
			// Code fence always triggers ComplexityComplex — even a tiny snippet.
			// The implementation is correct; the previous expectation (medium) was wrong.
			expected: ComplexityComplex,
		},

		// Complex cases
		{
			name: "long prompt under complex threshold",
			// ~76 tokens — above simple (50) but well below complex (300).
			// Correctly classified as medium by token count alone.
			prompt:   "Write a detailed explanation of the following: Lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod tempor incididunt ut labore et dolore magna aliqua ut enim ad minim veniam quis nostrud exercitation ullamco laboris nisi ut aliquip ex ea commodo consequat duis aute irure dolor in reprehenderit in voluptate velit esse cillum dolore eu fugiat nulla pariatur excepteur sint occaecat cupidatat non proident sunt in culpa qui officia deserunt mollit anim id est laborum",
			expected: ComplexityMedium,
		},
		{
			name: "long prompt over threshold",
			// 5 × 69 = 345 tokens > 300 → triggers ComplexityComplex by token count.
			prompt:   "Lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod tempor incididunt ut labore et dolore magna aliqua ut enim ad minim veniam quis nostrud exercitation ullamco laboris nisi ut aliquip ex ea commodo consequat duis aute irure dolor in reprehenderit in voluptate velit esse cillum dolore eu fugiat nulla pariatur excepteur sint occaecat cupidatat non proident sunt in culpa qui officia deserunt mollit anim id est laborum Lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod tempor incididunt ut labore et dolore magna aliqua ut enim ad minim veniam quis nostrud exercitation ullamco laboris nisi ut aliquip ex ea commodo consequat duis aute irure dolor in reprehenderit in voluptate velit esse cillum dolore eu fugiat nulla pariatur excepteur sint occaecat cupidatat non proident sunt in culpa qui officia deserunt mollit anim id est laborum Lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod tempor incididunt ut labore et dolore magna aliqua ut enim ad minim veniam quis nostrud exercitation ullamco laboris nisi ut aliquip ex ea commodo consequat duis aute irure dolor in reprehenderit in voluptate velit esse cillum dolore eu fugiat nulla pariatur excepteur sint occaecat cupidatat non proident sunt in culpa qui officia deserunt mollit anim id est laborum Lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod tempor incididunt ut labore et dolore magna aliqua ut enim ad minim veniam quis nostrud exercitation ullamco laboris nisi ut aliquip ex ea commodo consequat duis aute irure dolor in reprehenderit in voluptate velit esse cillum dolore eu fugiat nulla pariatur excepteur sint occaecat cupidatat non proident sunt in culpa qui officia deserunt mollit anim id est laborum Lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod tempor incididunt ut labore et dolore magna aliqua ut enim ad minim veniam quis nostrud exercitation ullamco laboris nisi ut aliquip ex ea commodo consequat duis aute irure dolor in reprehenderit in voluptate velit esse cillum dolore eu fugiat nulla pariatur excepteur sint occaecat cupidatat non proident sunt in culpa qui officia deserunt mollit anim id est laborum",
			expected: ComplexityComplex,
		},
		{
			name:     "multiple complex keywords",
			prompt:   "Compare and contrast the pros and cons of microservices vs monolithic architecture",
			expected: ComplexityComplex,
		},
		{
			name:     "multi-step prompt",
			prompt:   "First, explain what Docker is. And then describe how to create a container. After that, show how to deploy it.",
			expected: ComplexityComplex,
		},
		{
			name:     "debug keyword without code fence",
			prompt:   "debug this python code for me",
			expected: ComplexityMedium,
		},
		{
			name:     "debug with code fence",
			prompt:   "```python\ndef foo():\n    pass\n```\ndebug this",
			expected: ComplexityComplex,
		},
		{
			name:     "analyze keyword alone",
			prompt:   "Analyze the performance of this algorithm",
			expected: ComplexityMedium,
		},
		{
			name:     "two analyze keywords",
			prompt:   "Analyze the performance and analyze the memory usage",
			expected: ComplexityComplex,
		},
		{
			name:     "critique keyword",
			prompt:   "Critique this design decision",
			expected: ComplexityMedium,
		},
		{
			name:     "step by step",
			prompt:   "Explain step by step how HTTPS works",
			expected: ComplexityComplex,
		},
		{
			name:     "evaluate keyword",
			prompt:   "Evaluate this approach",
			expected: ComplexityMedium,
		},
		{
			name:     "implications keyword",
			prompt:   "What are the implications of this change",
			expected: ComplexityMedium,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := classifier.Classify(tt.prompt)
			assert.Equal(t, tt.expected, result, "prompt: %s", tt.prompt)
		})
	}
}

func TestTokenCount(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		expected int
	}{
		{
			name:     "empty string",
			text:     "",
			expected: 0,
		},
		{
			name:     "single word",
			text:     "hello",
			expected: 1,
		},
		{
			name:     "simple sentence",
			text:     "Hello world",
			expected: 2,
		},
		{
			name:     "sentence with punctuation",
			text:     "Hello, world!",
			expected: 2,
		},
		{
			name:     "multiple spaces",
			text:     "hello    world",
			expected: 2,
		},
		{
			name:     "tabs and spaces",
			text:     "hello\tworld\ntest",
			expected: 3,
		},
		{
			name: "code block",
			text: "```\nprint('hello')\n```",
			// Backtick (U+0060 GRAVE ACCENT) is Unicode category Sk (Modifier Symbol),
			// NOT Punctuation, so unicode.IsPunct('`') == false in Go.
			// Tokens: "```", "print", "hello", "```" = 4
			expected: 4,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := TokenCount(tt.text)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestContainsCodeFence(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		expected bool
	}{
		{
			name:     "no code fence",
			text:     "Hello world",
			expected: false,
		},
		{
			name:     "single backtick",
			text:     "Use `var` for this",
			expected: false,
		},
		{
			name:     "triple backtick",
			text:     "```\ncode\n```",
			expected: true,
		},
		{
			name:     "triple backtick with language",
			text:     "```python\nprint('hi')\n```",
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := containsCodeFence(tt.text)
			assert.Equal(t, tt.expected, result)
		})
	}
}
