package ai

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Anthropic Messages Batch API endpoints. Batch is generally available — no
// beta header needed beyond the standard anthropic-version.
const (
	anthropicBatchEndpoint = "https://api.anthropic.com/v1/messages/batches"
)

// BatchItem is one request in a batch. CustomID is opaque to Anthropic but
// must be unique within a batch — we use it to map results back to recipes.
type BatchItem struct {
	CustomID    string
	UserContent string
}

// BatchCounts mirrors the request_counts field on a batch.
type BatchCounts struct {
	Processing int
	Succeeded  int
	Errored    int
	Canceled   int
	Expired    int
}

// BatchStatus is everything the poller needs to know about a batch on a given
// tick. ResultsURL is set once ProcessingStatus flips to "ended".
type BatchStatus struct {
	BatchID          string
	ProcessingStatus string
	Counts           BatchCounts
	ResultsURL       string
	EndedAt          *time.Time
}

// BatchResultKind matches the Anthropic per-request result type field.
type BatchResultKind string

const (
	BatchResultSucceeded BatchResultKind = "succeeded"
	BatchResultErrored   BatchResultKind = "errored"
	BatchResultCanceled  BatchResultKind = "canceled"
	BatchResultExpired   BatchResultKind = "expired"
)

// BatchResultItem is one parsed JSONL line from the results stream. For
// succeeded results Content holds the concatenated text from the message.
type BatchResultItem struct {
	CustomID string
	Kind     BatchResultKind
	Content  string
	Error    string
}

// BatchProvider is the optional capability for providers that support
// asynchronous batch processing. Only anthropic implements it today —
// callers should type-assert and fall back to the sync Provider.Process flow
// when this returns nil.
type BatchProvider interface {
	SubmitBatch(systemPrompt string, items []BatchItem) (*BatchStatus, error)
	PollBatch(batchID string) (*BatchStatus, error)
	FetchBatchResults(resultsURL string) ([]BatchResultItem, error)
}

// AsBatchProvider returns p as a BatchProvider, or nil if it doesn't support
// batching. Use this to feature-detect at the call site instead of letting a
// type assertion panic in user code.
func AsBatchProvider(p Provider) BatchProvider {
	if bp, ok := p.(BatchProvider); ok {
		return bp
	}
	return nil
}

// --- Anthropic batch wire types ---

type anthropicBatchParams struct {
	Model       string                 `json:"model"`
	MaxTokens   int                    `json:"max_tokens"`
	Temperature float64                `json:"temperature"`
	System      []anthropicSystemBlock `json:"system"`
	Messages    []anthropicMessage     `json:"messages"`
}

type anthropicBatchRequest struct {
	CustomID string               `json:"custom_id"`
	Params   anthropicBatchParams `json:"params"`
}

type anthropicBatchSubmit struct {
	Requests []anthropicBatchRequest `json:"requests"`
}

type anthropicBatchCounts struct {
	Processing int `json:"processing"`
	Succeeded  int `json:"succeeded"`
	Errored    int `json:"errored"`
	Canceled   int `json:"canceled"`
	Expired    int `json:"expired"`
}

type anthropicBatch struct {
	ID               string               `json:"id"`
	Type             string               `json:"type"`
	ProcessingStatus string               `json:"processing_status"`
	RequestCounts    anthropicBatchCounts `json:"request_counts"`
	ResultsURL       string               `json:"results_url"`
	EndedAt          *time.Time           `json:"ended_at"`
	Error            *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type anthropicBatchResultLine struct {
	CustomID string `json:"custom_id"`
	Result   struct {
		Type    string `json:"type"`
		Message *struct {
			Content []anthropicBlock `json:"content"`
		} `json:"message"`
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	} `json:"result"`
}

// SubmitBatch ships a batch off to Anthropic and returns the batch handle.
// The system prompt is sent once per request (the API doesn't share system
// across requests) but tagged with cache_control so every entry after the
// first to hit the worker reads from cache.
func (c *anthropicClient) SubmitBatch(systemPrompt string, items []BatchItem) (*BatchStatus, error) {
	if len(items) == 0 {
		return nil, fmt.Errorf("anthropic batch: empty items")
	}

	reqs := make([]anthropicBatchRequest, 0, len(items))
	for _, it := range items {
		reqs = append(reqs, anthropicBatchRequest{
			CustomID: it.CustomID,
			Params: anthropicBatchParams{
				Model:       c.model,
				MaxTokens:   c.maxTokens,
				Temperature: c.temperature,
				System: []anthropicSystemBlock{{
					Type:         "text",
					Text:         systemPrompt,
					CacheControl: &anthropicCacheControl{Type: "ephemeral"},
				}},
				Messages: []anthropicMessage{{Role: "user", Content: it.UserContent}},
			},
		})
	}

	body, err := json.Marshal(anthropicBatchSubmit{Requests: reqs})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, anthropicBatchEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	// Batch submission can include large bodies — give it room to upload.
	httpClient := &http.Client{Timeout: 5 * time.Minute}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic batch submit: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("anthropic batch submit status %d: %s", resp.StatusCode, string(respBody))
	}

	var parsed anthropicBatch
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("decode batch submit response: %w (body: %.200q)", err, string(respBody))
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("anthropic batch error: %s", parsed.Error.Message)
	}
	return toBatchStatus(&parsed), nil
}

// PollBatch fetches the current status of an in-progress batch. It does not
// download results — the caller checks ProcessingStatus and only follows up
// with FetchBatchResults when it's "ended".
func (c *anthropicClient) PollBatch(batchID string) (*BatchStatus, error) {
	req, err := http.NewRequest(http.MethodGet, anthropicBatchEndpoint+"/"+batchID, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic batch poll: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("anthropic batch poll status %d: %s", resp.StatusCode, string(body))
	}

	var parsed anthropicBatch
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode batch poll: %w", err)
	}
	return toBatchStatus(&parsed), nil
}

// FetchBatchResults downloads the JSONL results stream and parses one entry
// per line. The stream can be large (one entry per recipe) but we keep it in
// memory because batches in this app are small (≤ a few dozen).
func (c *anthropicClient) FetchBatchResults(resultsURL string) ([]BatchResultItem, error) {
	if resultsURL == "" {
		return nil, fmt.Errorf("anthropic batch results: empty url")
	}
	req, err := http.NewRequest(http.MethodGet, resultsURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	httpClient := &http.Client{Timeout: 5 * time.Minute}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic batch results: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("anthropic batch results status %d: %s", resp.StatusCode, string(body))
	}

	var out []BatchResultItem
	// Lines can carry an entire localized recipe — bump the scanner buffer
	// well above bufio's 64 KiB default so we don't truncate.
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var parsed anthropicBatchResultLine
		if err := json.Unmarshal(line, &parsed); err != nil {
			return nil, fmt.Errorf("decode result line: %w", err)
		}
		item := BatchResultItem{
			CustomID: parsed.CustomID,
			Kind:     BatchResultKind(parsed.Result.Type),
		}
		switch item.Kind {
		case BatchResultSucceeded:
			if parsed.Result.Message != nil {
				var buf bytes.Buffer
				for _, blk := range parsed.Result.Message.Content {
					if blk.Type == "text" {
						buf.WriteString(blk.Text)
					}
				}
				item.Content = buf.String()
			}
		case BatchResultErrored:
			if parsed.Result.Error != nil {
				item.Error = parsed.Result.Error.Message
			}
		case BatchResultCanceled:
			item.Error = "canceled"
		case BatchResultExpired:
			item.Error = "expired"
		}
		out = append(out, item)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan results: %w", err)
	}
	return out, nil
}

func toBatchStatus(b *anthropicBatch) *BatchStatus {
	return &BatchStatus{
		BatchID:          b.ID,
		ProcessingStatus: b.ProcessingStatus,
		Counts: BatchCounts{
			Processing: b.RequestCounts.Processing,
			Succeeded:  b.RequestCounts.Succeeded,
			Errored:    b.RequestCounts.Errored,
			Canceled:   b.RequestCounts.Canceled,
			Expired:    b.RequestCounts.Expired,
		},
		ResultsURL: b.ResultsURL,
		EndedAt:    b.EndedAt,
	}
}
