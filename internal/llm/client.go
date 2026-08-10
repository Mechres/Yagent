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
	Role       string     `json:"role"` // "system" | "user" | "assistant" | "tool"
	Content    string     `json:"content"`
	ToolCallID string     `json:"tool_call_id,omitempty"` // set on role="tool" results
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`   // set on assistant messages that invoke tools
}

// ToolCall is one tool invocation requested by the model, in OpenAI wire format.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"` // always "function"
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction names the tool and carries the raw JSON argument object.
type ToolCallFunction struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Response is a full (non-streamed) assistant reply, including any tool calls.
type Response struct {
	Message   Message
	ToolCalls []ToolCall
}

// ToolSchema is the OpenAI tools-API function schema sent in the request.
// Keep schemas small: name + one-line description + a few args.
type ToolSchema struct {
	Type     string `json:"type"` // always "function"
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
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
	Model    string       `json:"model"`
	Messages []Message    `json:"messages"`
	Stream   bool         `json:"stream"`
	Tools    []ToolSchema `json:"tools,omitempty"`
}

// chatChunk is one streaming delta from /v1/chat/completions.
type chatChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
}

// ChatStream sends messages (plus optional tool schemas) to the model with
// streaming enabled, calls onDelta for each content fragment as it arrives,
// and returns the assembled response including any tool calls. It retries
// transport errors up to 3 times with backoff; HTTP error statuses are
// returned as errors immediately.
func (c *Client) ChatStream(ctx context.Context, messages []Message, tools []ToolSchema, onDelta func(string)) (*Response, error) {
	reqBody := chatCompletionRequest{Model: c.Model, Messages: messages, Stream: true, Tools: tools}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(backoffSchedule[attempt-1]):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		resp, err := c.chatStreamOnce(ctx, body, onDelta)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// Only transport-level errors are retried; a server response with a
		// non-2xx status is deterministic and would just fail again.
		if isHTTPError(err) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("chat stream failed after %d attempts: %w", maxRetries, lastErr)
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

func (c *Client) chatStreamOnce(ctx context.Context, body []byte, onDelta func(string)) (*Response, error) {
	url := strings.TrimRight(c.ServerURL, "/") + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, &httpStatusError{status: resp.StatusCode, body: strings.TrimSpace(string(msg))}
	}

	respMessage := &Response{}
	err = ParseSSE(resp.Body, func(data string) error {
		if data == "" {
			return nil
		}
		var chunk chatChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return fmt.Errorf("decode chunk: %w", err)
		}
		if len(chunk.Choices) == 0 {
			return nil
		}
		delta := chunk.Choices[0].Delta
		if delta.Content != "" {
			respMessage.Message.Content += delta.Content
			onDelta(delta.Content)
		}
		for _, tc := range delta.ToolCalls {
			// Tool calls arrive fragmented across chunks; accumulate by index.
			for len(respMessage.ToolCalls) <= tc.Index {
				respMessage.ToolCalls = append(respMessage.ToolCalls, ToolCall{Type: "function"})
			}
			call := &respMessage.ToolCalls[tc.Index]
			if tc.ID != "" {
				call.ID = tc.ID
			}
			if tc.Function.Name != "" {
				call.Function.Name = tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				call.Function.Arguments = append(call.Function.Arguments, tc.Function.Arguments...)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read stream: %w", err)
	}
	respMessage.Message.Role = "assistant"
	respMessage.Message.ToolCalls = respMessage.ToolCalls
	return respMessage, nil
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
