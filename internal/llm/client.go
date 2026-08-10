package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Message is one chat message in the OpenAI-compatible format.
type Message struct {
	Role    string `json:"role"` // "system" | "user" | "assistant"
	Content string `json:"content"`
}

// Client talks to an OpenAI-compatible local inference server.
type Client struct {
	ServerURL string // base URL, e.g. http://localhost:11434; /v1 is appended by calls
	Model     string
	HTTP      *http.Client
}

// NewClient constructs a Client with default HTTP settings.
func NewClient(serverURL, model string) *Client {
	return &Client{
		ServerURL: serverURL,
		Model:     model,
		HTTP:      &http.Client{},
	}
}

// maxRetries and backoff schedule for transport errors. HTTP error statuses
// (4xx/5xx) are NOT retried.
const maxRetries = 3

var backoffSchedule = []time.Duration{500 * time.Millisecond, 1 * time.Second, 2 * time.Second}

type chatCompletionRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
}

// chatChunk is one streaming delta from /v1/chat/completions.
type chatChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
}

// ChatStream sends messages to the model with streaming enabled and calls
// onDelta for each content fragment as it arrives, in order. It retries
// transport errors up to 3 times with backoff; HTTP error statuses are
// returned as errors immediately.
func (c *Client) ChatStream(ctx context.Context, messages []Message, onDelta func(string)) error {
	reqBody := chatCompletionRequest{Model: c.Model, Messages: messages, Stream: true}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(backoffSchedule[attempt-1]):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		lastErr = c.chatStreamOnce(ctx, body, onDelta)
		if lastErr == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Only transport-level errors are retried; a server response with a
		// non-2xx status is deterministic and would just fail again.
		if isHTTPError(lastErr) {
			return lastErr
		}
	}
	return fmt.Errorf("chat stream failed after %d attempts: %w", maxRetries, lastErr)
}

// isHTTPError reports whether err came from a non-2xx HTTP response
// (as opposed to a transport/parse error worth retrying).
func isHTTPError(err error) bool {
	_, ok := err.(*httpStatusError)
	return ok
}

type httpStatusError struct {
	status int
	body   string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("server returned %s: %s", http.StatusText(e.status), e.body)
}

func (c *Client) chatStreamOnce(ctx context.Context, body []byte, onDelta func(string)) error {
	url := strings.TrimRight(c.ServerURL, "/") + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("request %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &httpStatusError{status: resp.StatusCode, body: strings.TrimSpace(string(msg))}
	}

	var full string
	err = ParseSSE(resp.Body, func(data string) error {
		if data == "" {
			return nil
		}
		var chunk chatChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return fmt.Errorf("decode chunk: %w", err)
		}
		if len(chunk.Choices) > 0 {
			delta := chunk.Choices[0].Delta.Content
			full += delta
			onDelta(delta)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("read stream: %w", err)
	}
	if full == "" {
		return fmt.Errorf("empty response from %s", url)
	}
	return nil
}

// Embed returns a fixed stub embedding; real embeddings (nomic-embed-text)
// arrive in M3.
func (c *Client) Embed(ctx context.Context, text string) ([]float32, error) {
	if ctx == nil {
		return nil, fmt.Errorf("nil context")
	}
	_ = text
	return []float32{0, 0, 0}, nil
}
