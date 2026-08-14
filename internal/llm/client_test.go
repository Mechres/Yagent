package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func sseServer(t *testing.T, events ...string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatalf("server does not support http.Flusher")
		}
		for _, e := range events {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", e)
			flusher.Flush()
		}
	}))
}

func chunkData(content string) string {
	b, _ := json.Marshal(chatChunk{Choices: []struct {
		Delta struct {
			Content          string `json:"content"`
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
	}{{Delta: struct {
		Content          string `json:"content"`
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
	}{Content: content}}}})
	return string(b)
}

// toolCallChunk builds a raw SSE data payload for a tool_calls delta fragment.
func toolCallChunk(index int, id, name, argsFrag string) string {
	raw := fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":%d,"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}]}}]}`,
		index, id, name, argsFrag)
	return raw
}

func TestChatStreamDeliversReasoning(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		raw := []string{
			`{"choices":[{"delta":{"reasoning_content":"let me think "}}]}`,
			`{"choices":[{"delta":{"reasoning_content":"about it"}}]}`,
			`{"choices":[{"delta":{"content":"The answer is 42"}}]}`,
		}
		for _, e := range raw {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", e)
			flusher.Flush()
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "test-model")
	var reasoning, content []string
	resp, err := client.ChatStream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil,
		func(d string) { content = append(content, d) },
		func(r string) { reasoning = append(reasoning, r) })
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if got := strings.Join(reasoning, ""); got != "let me think about it" {
		t.Errorf("reasoning = %q", got)
	}
	if got := strings.Join(content, ""); got != "The answer is 42" {
		t.Errorf("content deltas = %q", got)
	}
	// reasoning must NOT leak into the assembled content or content deltas
	if resp.Message.Content != "The answer is 42" {
		t.Errorf("content = %q, reasoning leaked into the answer", resp.Message.Content)
	}
	if strings.Contains(resp.Message.Content, "think") {
		t.Error("reasoning leaked into resp.Message.Content")
	}
}

func TestChatStreamCollectsDeltas(t *testing.T) {
	t.Parallel()
	var gotReq atomic.Value
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q, want /v1/chat/completions", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		gotReq.Store(string(body))

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, content := range []string{"Hel", "lo ", "world"} {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", chunkData(content))
			flusher.Flush()
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "test-model")
	var deltas []string
	resp, err := client.ChatStream(context.Background(), []Message{
		{Role: "system", Content: "be brief"},
		{Role: "user", Content: "hi"},
	}, nil, func(d string) { deltas = append(deltas, d) }, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if got := strings.Join(deltas, ""); got != "Hello world" {
		t.Errorf("streamed text = %q, want %q", got, "Hello world")
	}
	if resp.Message.Content != "Hello world" || resp.Message.Role != "assistant" {
		t.Errorf("resp.Message = %+v", resp.Message)
	}
	if len(resp.ToolCalls) != 0 {
		t.Errorf("ToolCalls = %+v, want none", resp.ToolCalls)
	}

	var req chatCompletionRequest
	if err := json.Unmarshal([]byte(gotReq.Load().(string)), &req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if !req.Stream {
		t.Error("Stream = false, want true")
	}
	if req.Model != "test-model" {
		t.Errorf("Model = %q", req.Model)
	}
	if len(req.Messages) != 2 || req.Messages[1].Content != "hi" {
		t.Errorf("Messages = %+v", req.Messages)
	}
}

func TestChatStreamBearerToken(t *testing.T) {
	t.Parallel()
	var gotAuth atomic.Value
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		if r.URL.Path == "/v1/embeddings" {
			_, _ = fmt.Fprintf(w, `{"data":[{"embedding":[0.1,0.2]}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", chunkData("ok"))
		flusher.Flush()
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "test-model")
	client.BearerToken = "secret-key"
	if _, err := client.ChatStream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, func(string) {}, nil); err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if got := gotAuth.Load().(string); got != "Bearer secret-key" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer secret-key")
	}
	// embed path sends it too
	if _, err := client.Embed(context.Background(), "test-model", []string{"hello"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if got := gotAuth.Load().(string); got != "Bearer secret-key" {
		t.Errorf("embed Authorization = %q", got)
	}
}

func TestChatStreamSendsSampling(t *testing.T) {
	t.Parallel()
	var gotReq atomic.Value
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotReq.Store(string(body))
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", chunkData("ok"))
		flusher.Flush()
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "test-model")
	client.Sampling = Sampling{Temperature: 0.6, TopP: 0.95, TopK: 20, RepetitionPenalty: 1.05}
	if _, err := client.ChatStream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, func(string) {}, nil); err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	var req chatCompletionRequest
	if err := json.Unmarshal([]byte(gotReq.Load().(string)), &req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if req.Temperature != 0.6 || req.TopP != 0.95 || req.TopK != 20 || req.RepetitionPenalty != 1.05 {
		t.Errorf("sampling not forwarded: %+v", req.Sampling)
	}

	// zero values are omitted (cloud endpoints that reject top_k/rep_penalty)
	client.Sampling = Sampling{Temperature: 0.6, TopP: 0.95}
	if _, err := client.ChatStream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, func(string) {}, nil); err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	body := gotReq.Load().(string)
	if strings.Contains(body, "top_k") || strings.Contains(body, "repetition_penalty") {
		t.Errorf("zero sampling fields should be omitted: %s", body)
	}
}

func TestChatStreamSendsToolSchemas(t *testing.T) {
	t.Parallel()
	var gotReq atomic.Value
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotReq.Store(string(body))
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "test-model")
	_, err := client.ChatStream(context.Background(), []Message{{Role: "user", Content: "x"}},
		[]ToolSchema{{Type: "function", Function: struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			Parameters  map[string]any `json:"parameters"`
		}{Name: "fs_read", Description: "read a file", Parameters: map[string]any{
			"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}},
		}}}}, func(string) {}, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	var req chatCompletionRequest
	if err := json.Unmarshal([]byte(gotReq.Load().(string)), &req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if len(req.Tools) != 1 || req.Tools[0].Function.Name != "fs_read" {
		t.Fatalf("Tools = %+v", req.Tools)
	}
}

func TestChatStreamAccumulatesFragmentedToolCalls(t *testing.T) {
	t.Parallel()
	ts := sseServer(t,
		`{"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"fs_read","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"main.go\"}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_2","type":"function","function":{"name":"grep","arguments":"{\"pattern\":\"func \"}"}}]}}]}`,
		"[DONE]",
	)
	defer ts.Close()

	client := NewClient(ts.URL, "test-model")
	resp, err := client.ChatStream(context.Background(), []Message{{Role: "user", Content: "x"}}, nil, func(string) {}, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if len(resp.ToolCalls) != 2 {
		t.Fatalf("ToolCalls = %+v, want 2", resp.ToolCalls)
	}
	first := resp.ToolCalls[0]
	if first.ID != "call_1" || first.Function.Name != "fs_read" {
		t.Errorf("call 0 = %+v", first)
	}
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(first.Function.Arguments, &args); err != nil {
		t.Fatalf("call 0 args %q: %v", first.Function.Arguments, err)
	}
	if args.Path != "main.go" {
		t.Errorf("call 0 path = %q, want main.go", args.Path)
	}
	if resp.ToolCalls[1].Function.Name != "grep" {
		t.Errorf("call 1 = %+v", resp.ToolCalls[1])
	}
	if !json.Valid(first.Function.Arguments) {
		t.Errorf("call 0 arguments %q are not valid JSON", first.Function.Arguments)
	}
}

func TestChatStreamHTTPErrorNotRetried(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "model not found", http.StatusNotFound)
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "test-model")
	_, err := client.ChatStream(context.Background(), []Message{{Role: "user", Content: "x"}}, nil, func(string) {}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "Not Found") {
		t.Errorf("error = %v, want server status text", err)
	}
	if hits.Load() != 1 {
		t.Errorf("server hit %d times, want 1 (no retry on 4xx)", hits.Load())
	}
}

// failingRoundTripper fails the first n requests with a transport error,
// then delegates to the wrapped transport.
type failingRoundTripper struct {
	n      int32
	next   http.RoundTripper
	failed atomic.Int32
}

func (f *failingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if f.failed.Add(1) <= f.n {
		return nil, fmt.Errorf("dial tcp: connection refused")
	}
	return f.next.RoundTrip(req)
}

func TestChatStreamRetriesTransportErrors(t *testing.T) {
	t.Parallel()
	ts := sseServer(t, chunkData("ok"), "[DONE]")
	defer ts.Close()

	rt := &failingRoundTripper{n: 2, next: http.DefaultTransport}
	client := NewClient(ts.URL, "test-model")
	client.HTTP = &http.Client{Transport: rt}

	var deltas []string
	start := time.Now()
	resp, err := client.ChatStream(context.Background(), []Message{{Role: "user", Content: "x"}}, nil, func(d string) {
		deltas = append(deltas, d)
	}, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if got := strings.Join(deltas, ""); got != "ok" {
		t.Errorf("streamed text = %q, want %q", got, "ok")
	}
	if resp.Message.Content != "ok" {
		t.Errorf("Message.Content = %q", resp.Message.Content)
	}
	if rt.failed.Load() != 3 {
		t.Errorf("total attempts = %d, want 3 (2 failures + success)", rt.failed.Load())
	}
	if time.Since(start) < 1400*time.Millisecond {
		t.Errorf("retries did not back off: elapsed %v", time.Since(start))
	}
}

func TestEmbed(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("path = %q, want /v1/embeddings", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req["model"] != "nomic-embed-text" {
			t.Errorf("model = %v", req["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[
			{"object":"embedding","index":0,"embedding":[0.1,0.2,0.3]},
			{"object":"embedding","index":1,"embedding":[0.4,0.5,0.6]}]}`)
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "chat-model")
	got, err := client.Embed(context.Background(), "nomic-embed-text", []string{"alpha", "beta"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(got) != 2 || len(got[0]) != 3 || got[0][0] != 0.1 || got[1][2] != 0.6 {
		t.Errorf("vectors = %v", got)
	}
}

func TestEmbedHTTPError(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "embedding model not loaded", http.StatusNotFound)
	}))
	defer ts.Close()
	client := NewClient(ts.URL, "m")
	_, err := client.Embed(context.Background(), "nomic-embed-text", []string{"x"})
	if err == nil || !strings.Contains(err.Error(), "Not Found") {
		t.Errorf("err = %v", err)
	}
}

func TestChatStreamGivesUpAfterMaxRetries(t *testing.T) {
	t.Parallel()
	ts := sseServer(t, chunkData("never"), "[DONE]") // never reached
	defer ts.Close()

	rt := &failingRoundTripper{n: 100, next: http.DefaultTransport}
	client := NewClient(ts.URL, "test-model")
	client.HTTP = &http.Client{Transport: rt}

	_, err := client.ChatStream(context.Background(), []Message{{Role: "user", Content: "x"}}, nil, func(string) {}, nil)
	if err == nil {
		t.Fatal("expected error after max retries")
	}
	if !strings.Contains(err.Error(), "after 3 attempts") {
		t.Errorf("error = %v, want retry-exhausted message", err)
	}
}

func TestCountTokensLlamaCPPEndpoint(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tokenize" {
			t.Errorf("path = %q, want /tokenize", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if _, ok := req["content"]; !ok {
			t.Errorf("llama.cpp tokenize body = %v, want a content field", req)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"tokens":[1,2,3,4,5]}`)
	}))
	defer ts.Close()

	c := NewClient(ts.URL, "m")
	n, err := c.CountTokens(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	if n != 5 {
		t.Errorf("tokens = %d, want 5", n)
	}
}

func TestCountTokensOllamaFallback(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tokenize":
			http.Error(w, "not found", http.StatusNotFound)
		case "/api/tokenize":
			body, _ := io.ReadAll(r.Body)
			var req map[string]any
			if err := json.Unmarshal(body, &req); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			if req["model"] != "m" || req["prompt"] == "" {
				t.Errorf("ollama tokenize body = %v", req)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"tokens":[9,8,7]}`)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer ts.Close()

	c := NewClient(ts.URL, "m")
	n, err := c.CountTokens(context.Background(), "hey")
	if err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	if n != 3 {
		t.Errorf("tokens = %d, want 3", n)
	}
}

func TestCountTokensUnavailable(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer ts.Close()

	c := NewClient(ts.URL, "m")
	if _, err := c.CountTokens(context.Background(), "x"); err == nil {
		t.Fatal("expected an error when the server exposes no tokenizer")
	}
}

func TestBaseURLNormalizesV1Suffix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"http://localhost:8089", "http://localhost:8089"},
		{"http://localhost:8089/", "http://localhost:8089"},
		{"http://localhost:8089/v1", "http://localhost:8089"},
		{"https://integrate.api.nvidia.com/v1", "https://integrate.api.nvidia.com"},
		{"https://api.together.xyz/v1", "https://api.together.xyz"},
		{"https://api.mistral.ai/v1", "https://api.mistral.ai"},
		{"https://opencode.ai/zen/go", "https://opencode.ai/zen/go"},
	}
	for _, c := range cases {
		if got := baseURL(c.in); got != c.want {
			t.Errorf("baseURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// the full chat path resolves correctly for both shapes
	if got := baseURL("https://integrate.api.nvidia.com/v1") + "/v1/chat/completions"; got != "https://integrate.api.nvidia.com/v1/chat/completions" {
		t.Errorf("nvidia chat path = %q", got)
	}
	if got := baseURL("http://localhost:8089") + "/v1/chat/completions"; got != "http://localhost:8089/v1/chat/completions" {
		t.Errorf("llama.cpp chat path = %q", got)
	}
}
