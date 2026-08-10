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

// sseServer returns an httptest server that streams the given events as SSE.
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
			Content string `json:"content"`
		} `json:"delta"`
	}{{Delta: struct {
		Content string `json:"content"`
	}{Content: content}}}})
	return string(b)
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
	err := client.ChatStream(context.Background(), []Message{
		{Role: "system", Content: "be brief"},
		{Role: "user", Content: "hi"},
	}, func(d string) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if got := strings.Join(deltas, ""); got != "Hello world" {
		t.Errorf("streamed text = %q, want %q", got, "Hello world")
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

func TestChatStreamHTTPErrorNotRetried(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "model not found", http.StatusNotFound)
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "test-model")
	err := client.ChatStream(context.Background(), []Message{{Role: "user", Content: "x"}}, func(string) {})
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
	err := client.ChatStream(context.Background(), []Message{{Role: "user", Content: "x"}}, func(d string) {
		deltas = append(deltas, d)
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if got := strings.Join(deltas, ""); got != "ok" {
		t.Errorf("streamed text = %q, want %q", got, "ok")
	}
	if rt.failed.Load() != 3 {
		t.Errorf("total attempts = %d, want 3 (2 failures + success)", rt.failed.Load())
	}
	if time.Since(start) < 1400*time.Millisecond {
		t.Errorf("retries did not back off: elapsed %v", time.Since(start))
	}
}

func TestChatStreamGivesUpAfterMaxRetries(t *testing.T) {
	t.Parallel()
	ts := sseServer(t, chunkData("never"), "[DONE]") // never reached
	defer ts.Close()

	rt := &failingRoundTripper{n: 100, next: http.DefaultTransport}
	client := NewClient(ts.URL, "test-model")
	client.HTTP = &http.Client{Transport: rt}

	err := client.ChatStream(context.Background(), []Message{{Role: "user", Content: "x"}}, func(string) {})
	if err == nil {
		t.Fatal("expected error after max retries")
	}
	if !strings.Contains(err.Error(), "after 3 attempts") {
		t.Errorf("error = %v, want retry-exhausted message", err)
	}
}
