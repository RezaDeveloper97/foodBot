package ai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Groq exposes an OpenAI-compatible Chat Completions endpoint, so the wire
// format here is identical to OpenAI's — pointing this at api.openai.com would
// also work for an OpenAI key with no other changes.
const groqEndpoint = "https://api.groq.com/openai/v1/chat/completions"

type groqClient struct {
	apiKey    string
	model     string
	maxTokens int
	http      *http.Client
}

func newGroq(apiKey, model string, maxTokens int) *groqClient {
	return &groqClient{
		apiKey:    apiKey,
		model:     model,
		maxTokens: maxTokens,
		http:      &http.Client{Timeout: 90 * time.Second},
	}
}

type groqMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type groqRequest struct {
	Model     string        `json:"model"`
	MaxTokens int           `json:"max_tokens,omitempty"`
	Messages  []groqMessage `json:"messages"`
}

type groqChoice struct {
	Message      groqMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type groqResponse struct {
	Choices []groqChoice `json:"choices"`
	Error   *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

func (c *groqClient) Process(systemPrompt, userContent string) (string, error) {
	reqBody := groqRequest{
		Model:     c.model,
		MaxTokens: c.maxTokens,
		Messages: []groqMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userContent},
		},
	}
	b, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	httpReq, err := http.NewRequest(http.MethodPost, groqEndpoint, bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("groq request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("groq status %d: %s", resp.StatusCode, string(body))
	}

	var parsed groqResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("decode groq response: %w", err)
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("groq error: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("groq returned no choices")
	}

	text := parsed.Choices[0].Message.Content
	if text == "" {
		return "", fmt.Errorf("groq returned empty content (finish_reason=%q)",
			parsed.Choices[0].FinishReason)
	}
	return text, nil
}
