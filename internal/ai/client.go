package ai

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	thinkBlockRE = regexp.MustCompile(`(?s)<think>.*?</think>`)
	codeFenceRE  = regexp.MustCompile("(?s)^```(?:json)?\\s*(.*?)\\s*```$")
)

// CleanOutput strips reasoning-model artifacts (<think>...</think>) and
// surrounding code fences from a raw model response. Some Groq/DeepSeek/Qwen
// reasoning models leak their chain-of-thought into the response body, and
// many models wrap JSON output in ```json ... ``` fences.
func CleanOutput(s string) string {
	s = thinkBlockRE.ReplaceAllString(s, "")
	s = strings.TrimSpace(s)
	if m := codeFenceRE.FindStringSubmatch(s); m != nil {
		s = strings.TrimSpace(m[1])
	}
	return s
}

// Provider turns a system prompt + raw user content into the final localized
// text. Every concrete backend (Anthropic, Gemini, Groq, ...) implements this
// single method, so swapping the AI vendor is a one-line config change.
type Provider interface {
	Process(systemPrompt, userContent string) (string, error)
}

// Supported provider names accepted by New.
const (
	ProviderAnthropic = "anthropic"
	ProviderGemini    = "gemini"
	ProviderGroq      = "groq"
)

// New returns a Provider for the given name. Pass an empty model to use the
// sensible default for that provider.
func New(provider, apiKey, model string, maxTokens int) (Provider, error) {
	switch strings.ToLower(provider) {
	case ProviderAnthropic:
		if model == "" {
			model = "claude-sonnet-4-5"
		}
		return newAnthropic(apiKey, model, maxTokens), nil
	case ProviderGemini:
		if model == "" {
			model = "gemini-2.0-flash"
		}
		return newGemini(apiKey, model, maxTokens), nil
	case ProviderGroq:
		if model == "" {
			model = "llama-3.3-70b-versatile"
		}
		return newGroq(apiKey, model, maxTokens), nil
	default:
		return nil, fmt.Errorf("unknown AI provider %q (want %s|%s|%s)",
			provider, ProviderAnthropic, ProviderGemini, ProviderGroq)
	}
}
