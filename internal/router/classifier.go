// File: internal/router/classifier.go
// WHY: Classifies prompt complexity to route to the cheapest capable model.
// Uses token count and keyword detection (no heavy ML dependencies).

package router

import (
	"strings"
	"unicode"
)

// ComplexityLevel represents the complexity classification of a prompt.
type ComplexityLevel string

const (
	ComplexitySimple   ComplexityLevel = "simple"
	ComplexityMedium   ComplexityLevel = "medium"
	ComplexityComplex  ComplexityLevel = "complex"
)

// ComplexityClassifier classifies prompts by complexity.
type ComplexityClassifier struct {
	simpleTokenThreshold  int
	complexTokenThreshold int
	complexKeywords       []string
}

// NewComplexityClassifier creates a new ComplexityClassifier.
func NewComplexityClassifier(simpleThreshold, complexThreshold int) *ComplexityClassifier {
	return &ComplexityClassifier{
		simpleTokenThreshold:  simpleThreshold,
		complexTokenThreshold: complexThreshold,
		complexKeywords: []string{
			"analyze", "compare", "synthesize", "explain why", "step by step",
			"walk me through", "evaluate", "contrast", "pros and cons",
			"what are the implications", "critique", "debug", "and then", "after that",
		},
	}
}

// Classify determines the complexity level of a prompt.
func (c *ComplexityClassifier) Classify(prompt string) ComplexityLevel {
	tokenCount := TokenCount(prompt)
	hasCodeFence := containsCodeFence(prompt)
	keywordCount := c.countComplexKeywords(prompt)
	hasMultiStep := containsMultiStep(prompt)

	// Simple: token_count < threshold AND no code AND no complex keywords
	if tokenCount < c.simpleTokenThreshold && !hasCodeFence && keywordCount == 0 && !hasMultiStep {
		return ComplexitySimple
	}

	// Complex: token_count > threshold OR has code OR 2+ keywords OR multi-step
	if tokenCount > c.complexTokenThreshold || hasCodeFence || keywordCount >= 2 || hasMultiStep {
		return ComplexityComplex
	}

	// Everything else is medium
	return ComplexityMedium
}

// TokenCount returns a naive token count estimate.
// WHY: We don't use tiktoken - it requires CGO and adds 30MB to binary.
// Naive whitespace split is sufficient for classification accuracy.
func TokenCount(text string) int {
	words := strings.FieldsFunc(text, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsPunct(r)
	})
	return len(words)
}

// containsCodeFence checks if text contains a code fence.
func containsCodeFence(text string) bool {
	return strings.Contains(text, "```")
}

// countComplexKeywords counts how many complex keywords are in the text.
func (c *ComplexityClassifier) countComplexKeywords(text string) int {
	lowerText := strings.ToLower(text)
	count := 0
	for _, keyword := range c.complexKeywords {
		if strings.Contains(lowerText, keyword) {
			count++
		}
	}
	return count
}

// containsMultiStep checks for multi-step indicators.
func containsMultiStep(text string) bool {
	lowerText := strings.ToLower(text)
	multiStepPhrases := []string{"and then", "after that", "first ", "next ", "finally "}
	for _, phrase := range multiStepPhrases {
		if strings.Contains(lowerText, phrase) {
			return true
		}
	}
	return false
}
