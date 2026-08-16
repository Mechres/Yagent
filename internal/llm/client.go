package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// maxStreamToolCalls bounds how many parallel tool calls a single streamed
// assistant message may carry. OpenAI-compatible streams index tool_calls
// across chunks; a malformed stream with an absurd index (e.g. 1e9) would
// otherwise grow the accumulation slice to that size. 32 is far beyond any
// realistic single-message tool batch for a local agent.
const maxStreamToolCalls = 32

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
	// FinishReason is the server's terminal stop reason ("stop", "length",
	// "tool_calls", ...) captured from the final stream chunk. "length" means
	// the generation hit the context/limit cap and the reply is truncated.
	FinishReason string
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
	// BearerToken, when set, is sent as `Authorization: Bearer <token>` on
	// every request (used by the consult tool against cloud OpenAI-compatible
	// endpoints).
	BearerToken string
	// Sampling is forwarded on every chat request (zero values omitted).
	Sampling Sampling

	// tokenizeOnce/tokenizePath cache which tokenizer endpoint this server
	// exposes ("" = none), so CountTokens does not probe on every call.
	tokenizeOnce sync.Once
	tokenizePath string
	tokenizeErr  error
}

// NewClient constructs a Client with default HTTP settings.
// baseURL normalizes a configured server URL for the /v1/* suffixes: it strips
// a trailing "/v1" so chat/embed paths always resolve to <base>/v1/<endpoint>.
// Without this, a provider whose documented base already ends in /v1 (NVIDIA
// NIM, Together, Mistral) would produce /v1/v1/chat/completions -> 404.
func baseURL(serverURL string) string {
	u := strings.TrimRight(serverURL, "/")
	if strings.HasSuffix(u, "/v1") {
		u = strings.TrimSuffix(u, "/v1")
	}
	return u
}

func NewClient(serverURL, model string) *Client {
	return &Client{
		ServerURL: serverURL,
		Model:     model,
		HTTP:      &http.Client{},
	}
}

// Clone returns a new Client with the same server/model/auth/sampling but a
// fresh tokenizer probe and HTTP handle. Used to give a subagent child its own
// sampling (e.g. a role temperature) without sharing the parent's Client (which
// carries a sync.Once and must not be copied).
func (c *Client) Clone() *Client {
	nc := NewClient(c.ServerURL, c.Model)
	nc.HTTP = c.HTTP
	nc.BearerToken = c.BearerToken
	nc.Sampling = c.Sampling
	return nc
}

// Sampling holds generation parameters forwarded to the server on every chat
// request. Zero values are omitted, so servers that don't understand a field
// (some OpenAI-compatible cloud endpoints reject repetition_penalty/top_k/
// min_p) only receive what the user explicitly configured.
type Sampling struct {
	Temperature       float64 `json:"temperature,omitempty"`
	TopP              float64 `json:"top_p,omitempty"`
	TopK              int     `json:"top_k,omitempty"`
	RepetitionPenalty float64 `json:"repetition_penalty,omitempty"`
	MinP              float64 `json:"min_p,omitempty"`
	// ReasoningMaxTokens caps the model's reasoning (thinking) span per request
	// (llama.cpp/Ollama reasoning models). 0 = uncapped. Capping dramatically
	// speeds up long turns on a 12 GB card — each round-trip stops thinking and
	// answers.
	ReasoningMaxTokens int `json:"reasoning_max_tokens,omitempty"`
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
	Sampling `json:",inline"`
}

// chatChunk is one streaming delta from /v1/chat/completions.
type chatChunk struct {
	Choices []struct {
		// FinishReason arrives on the final chunk ("" mid-stream, "length" when
		// the generation hit the token cap, "stop"/"tool_calls" on completion).
		FinishReason string `json:"finish_reason"`
		Delta        struct {
			Content string `json:"content"`
			// ReasoningContent is the model's thinking span, streamed by
			// llama.cpp/Ollama for reasoning models (Qwen3.5/Qwythos). It is
			// surfaced via onReasoning and kept OUT of the assembled content
			// and history.
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
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
// streaming enabled, calls onDelta for each content fragment and onReasoning
// for each thinking fragment as they arrive, and returns the assembled
// response including any tool calls. It retries transport errors up to 3 times
// with backoff; HTTP error statuses are returned as errors immediately.
func (c *Client) ChatStream(ctx context.Context, messages []Message, tools []ToolSchema, onDelta, onReasoning func(string)) (*Response, error) {
	// Serialize against other inference on the same single-slot server
	// (subagents, consult, embeddings); a waiting request blocks, it never 500s.
	release, err := acquireSlot(ctx, c.ServerURL)
	if err != nil {
		return nil, err
	}
	defer release()
	reqBody := chatCompletionRequest{Model: c.Model, Messages: messages, Stream: true, Tools: tools, Sampling: c.Sampling}
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
		resp, err := c.chatStreamOnce(ctx, body, onDelta, onReasoning)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// Only transport-level errors are retried; a server response with a
		// non-2xx status is deterministic and would just fail again, and a
		// truncated stream is a signal for the agent to recover, not retry.
		if isHTTPError(err) || errors.Is(err, ErrStreamTruncated) {
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

func (c *Client) chatStreamOnce(ctx context.Context, body []byte, onDelta, onReasoning func(string)) (*Response, error) {
	url := baseURL(c.ServerURL) + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.BearerToken)
	}

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
		if chunk.Choices[0].FinishReason != "" {
			respMessage.FinishReason = chunk.Choices[0].FinishReason
		}
		delta := chunk.Choices[0].Delta
		if delta.ReasoningContent != "" {
			// Thinking is display-only: surfaced via onReasoning, never added
			// to the assembled content or to history.
			if onReasoning != nil {
				onReasoning(delta.ReasoningContent)
			}
		}
		if delta.Content != "" {
			respMessage.Message.Content += delta.Content
			onDelta(delta.Content)
		}
		for _, tc := range delta.ToolCalls {
			// Tool calls arrive fragmented across chunks; accumulate by index.
			// Guard the index: a malformed/partial OpenAI-compatible stream
			// (e.g. {"index": 1000000000}) would otherwise grow the slice to
			// that size before the agent's iteration cap could ever fire
			// (codex audit, 2026-08-16). Reject negative or absurd indices.
			if tc.Index < 0 || tc.Index >= maxStreamToolCalls {
				return fmt.Errorf("stream tool_call index %d out of range (max %d)", tc.Index, maxStreamToolCalls)
			}
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
	// A server that streams the terminal chunk with a finish_reason but closes
	// without "[DONE]" (llama.cpp/Ollama always send [DONE], but third-party
	// OpenAI-compatible endpoints occasionally omit it) is NOT truncation: the
	// reply is complete. Only a bare EOF with no terminal reason is truncation.
	if errors.Is(err, ErrStreamTruncated) && respMessage.FinishReason != "" {
		err = nil
	}
	if err != nil {
		return nil, fmt.Errorf("read stream: %w", err)
	}
	respMessage.Message.Role = "assistant"
	respMessage.Message.ToolCalls = respMessage.ToolCalls
	// Sanitize truncated tool-call arguments (invalid JSON) into a marker
	// object so the assistant message can be re-marshaled on the next request
	// instead of crashing the client.
	for i := range respMessage.ToolCalls {
		args := respMessage.ToolCalls[i].Function.Arguments
		if len(bytes.TrimSpace(args)) > 0 && !json.Valid(args) {
			respMessage.ToolCalls[i].Function.Arguments = json.RawMessage(TruncatedArgsMarker)
		}
	}
	return respMessage, nil
}

// Embed requests embeddings for texts from /v1/embeddings (e.g. the
// nomic-embed-text model) and returns them in input order.
func (c *Client) Embed(ctx context.Context, model string, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	release, err := acquireSlot(ctx, c.ServerURL)
	if err != nil {
		return nil, err
	}
	defer release()
	reqBody, err := json.Marshal(map[string]any{"model": model, "input": texts})
	if err != nil {
		return nil, fmt.Errorf("marshal embed request: %w", err)
	}
	url := baseURL(c.ServerURL) + "/v1/embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("build embed request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.BearerToken)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed request %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, &httpStatusError{status: resp.StatusCode, body: strings.TrimSpace(string(msg))}
	}

	var out struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode embed response: %w", err)
	}
	if len(out.Data) != len(texts) {
		return nil, fmt.Errorf("embed response has %d vectors for %d inputs", len(out.Data), len(texts))
	}
	vectors := make([][]float32, 0, len(out.Data))
	for _, d := range out.Data {
		vectors = append(vectors, d.Embedding)
	}
	return vectors, nil
}

// truncatedArgsMarker replaces a tool call whose arguments stream was cut off
// (invalid JSON) with a valid object the decoder recognizes, so the message can
// be re-serialized and the model gets the "re-emit" feedback instead of a
// marshal crash.
const TruncatedArgsMarker = `{"__truncated":true}`

// CountTokens returns the number of tokens the server's tokenizer assigns to
// text. It supports llama.cpp's root /tokenize endpoint (the dev llama-server
// on :8089) and Ollama's /api/tokenize. It returns an error when the server
// exposes neither — callers (e.g. agent.tokensFor) fall back to len/4 then.
// Every call is bounded by a 5s timeout so a slow server never stalls a turn.
func (c *Client) CountTokens(ctx context.Context, text string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	release, err := acquireSlot(ctx, c.ServerURL)
	if err != nil {
		return 0, err
	}
	defer release()
	c.tokenizeOnce.Do(func() {
		if n, ok := c.tryTokenize(ctx, "tokenize", map[string]any{"content": "probe"}); ok && n >= 0 {
			c.tokenizePath = "tokenize"
			return
		}
		if n, ok := c.tryTokenize(ctx, "api/tokenize", map[string]any{"model": c.Model, "prompt": "probe"}); ok && n >= 0 {
			c.tokenizePath = "api/tokenize"
			return
		}
		c.tokenizeErr = errors.New("server exposes no tokenizer (no /tokenize or /api/tokenize)")
	})
	if c.tokenizePath == "" {
		return 0, c.tokenizeErr
	}
	if c.tokenizePath == "tokenize" {
		n, ok := c.tryTokenize(ctx, "tokenize", map[string]any{"content": text})
		if !ok {
			return 0, c.tokenizeErr
		}
		return n, nil
	}
	n, ok := c.tryTokenize(ctx, "api/tokenize", map[string]any{"model": c.Model, "prompt": text})
	if !ok {
		return 0, c.tokenizeErr
	}
	return n, nil
}

// ServerProps is best-effort llama.cpp /props data.
type ServerProps struct {
	NCtx  int // the server's real context window (default_generation_settings.n_ctx)
	Slots int // total generation slots
	// CacheK / CacheV are the KV cache quantization types (e.g. "q8_0", "f16")
	// when the server exposes them (agy #5 — newer llama.cpp builds).
	CacheK string
	CacheV string
}

// ProbeServerProps fetches llama.cpp /props (P2 context-window autodetect).
// ok=false when the server doesn't expose it (e.g. Ollama).
func (c *Client) ProbeServerProps(ctx context.Context) (ServerProps, bool) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	url := strings.TrimRight(c.ServerURL, "/") + "/props"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ServerProps{}, false
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return ServerProps{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ServerProps{}, false
	}
	var props struct {
		TotalSlots                int `json:"total_slots"`
		DefaultGenerationSettings struct {
			NCtx       int     `json:"n_ctx"`
			CacheTypeK *string `json:"cache_type_k,omitempty"`
			CacheTypeV *string `json:"cache_type_v,omitempty"`
		} `json:"default_generation_settings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&props); err != nil {
		return ServerProps{}, false
	}
	p := ServerProps{
		NCtx:  props.DefaultGenerationSettings.NCtx,
		Slots: props.TotalSlots,
	}
	if props.DefaultGenerationSettings.CacheTypeK != nil {
		p.CacheK = *props.DefaultGenerationSettings.CacheTypeK
	}
	if props.DefaultGenerationSettings.CacheTypeV != nil {
		p.CacheV = *props.DefaultGenerationSettings.CacheTypeV
	}
	return p, props.DefaultGenerationSettings.NCtx > 0
}

// tryTokenize POSTs a tokenize request and returns (count, ok). ok=false means
// the endpoint is missing or returned something unexpected (the caller should
// try another path, or give up).
func (c *Client) tryTokenize(ctx context.Context, path string, body any) (int, bool) {
	payload, err := json.Marshal(body)
	if err != nil {
		return 0, false
	}
	url := strings.TrimRight(c.ServerURL, "/") + "/" + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return 0, false
	}
	req.Header.Set("Content-Type", "application/json")
	if c.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.BearerToken)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	var out struct {
		Tokens []int `json:"tokens"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, false
	}
	return len(out.Tokens), true
}
