package ai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

const anthropicEndpoint = "https://api.anthropic.com/v1/messages"

type anthropicClient struct {
	apiKey      string
	model       string
	maxTokens   int
	temperature float64
	http        *http.Client
}

func newAnthropic(apiKey, model string, maxTokens int, temperature float64) *anthropicClient {
	return &anthropicClient{
		apiKey:      apiKey,
		model:       model,
		maxTokens:   maxTokens,
		temperature: temperature,
		http:        &http.Client{Timeout: 90 * time.Second},
	}
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// anthropicCacheControl marks a system block for prompt caching. With
// "ephemeral" the block is cached for ~5 minutes; since a full fetch batch
// runs back-to-back, every recipe after the first in a batch — plus every
// refine retry — reads the system prompt from cache for ~90% off.
type anthropicCacheControl struct {
	Type string `json:"type"`
}

type anthropicSystemBlock struct {
	Type         string                 `json:"type"`
	Text         string                 `json:"text"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

type anthropicRequest struct {
	Model       string                 `json:"model"`
	MaxTokens   int                    `json:"max_tokens"`
	Temperature float64                `json:"temperature"`
	System      []anthropicSystemBlock `json:"system"`
	Messages    []anthropicMessage     `json:"messages"`
}

type anthropicBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

type anthropicResponse struct {
	Content []anthropicBlock `json:"content"`
	Usage   anthropicUsage   `json:"usage"`
	Error   *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *anthropicClient) Process(systemPrompt, userContent string) (string, error) {
	reqBody := anthropicRequest{
		Model:       c.model,
		MaxTokens:   c.maxTokens,
		Temperature: c.temperature,
		System: []anthropicSystemBlock{{
			Type:         "text",
			Text:         systemPrompt,
			CacheControl: &anthropicCacheControl{Type: "ephemeral"},
		}},
		Messages: []anthropicMessage{{Role: "user", Content: userContent}},
	}
	b, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	httpReq, err := http.NewRequest(http.MethodPost, anthropicEndpoint, bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("anthropic request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("anthropic status %d: %s", resp.StatusCode, string(body))
	}

	var parsed anthropicResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("decode anthropic response: %w", err)
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("anthropic error: %s", parsed.Error.Message)
	}

	// Surface cache effectiveness — on a healthy batch the first call shows
	// creation>0 and every subsequent call within the TTL shows read>0.
	log.Printf("[ai:anthropic] tokens in=%d out=%d cache_created=%d cache_read=%d",
		parsed.Usage.InputTokens, parsed.Usage.OutputTokens,
		parsed.Usage.CacheCreationInputTokens, parsed.Usage.CacheReadInputTokens)

	var out bytes.Buffer
	for _, blk := range parsed.Content {
		if blk.Type == "text" {
			out.WriteString(blk.Text)
		}
	}
	text := out.String()
	if text == "" {
		return "", fmt.Errorf("anthropic returned empty content")
	}
	return text, nil
}
