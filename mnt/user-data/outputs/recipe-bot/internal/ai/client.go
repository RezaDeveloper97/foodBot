package ai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const endpoint = "https://api.anthropic.com/v1/messages"

// Client wraps the Anthropic Messages API. Its single job here is to turn a
// raw recipe (as JSON) into a finished, localized Persian channel post.
type Client struct {
	apiKey    string
	model     string
	maxTokens int
	http      *http.Client
}

// New builds a client. model and maxTokens come from config so they can be
// tuned without recompiling.
func New(apiKey, model string, maxTokens int) *Client {
	return &Client{
		apiKey:    apiKey,
		model:     model,
		maxTokens: maxTokens,
		http:      &http.Client{Timeout: 90 * time.Second},
	}
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type request struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	System    string    `json:"system"`
	Messages  []message `json:"messages"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type response struct {
	Content []contentBlock `json:"content"`
	Error   *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Process sends the system prompt plus the raw recipe content and returns the
// model's final text output.
func (c *Client) Process(systemPrompt, userContent string) (string, error) {
	reqBody := request{
		Model:     c.model,
		MaxTokens: c.maxTokens,
		System:    systemPrompt,
		Messages:  []message{{Role: "user", Content: userContent}},
	}
	b, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	httpReq, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(b))
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

	var parsed response
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("decode anthropic response: %w", err)
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("anthropic error: %s", parsed.Error.Message)
	}

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
