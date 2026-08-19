// Package testkit contains deterministic local test servers for model-loop
// tests. It is deliberately independent of the agent package so lower-level
// llm, eval, and integration tests can share the same fault scripts.
package testkit

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Step describes one response to one request. Events are raw JSON SSE payloads
// (without the `data:` prefix). A step always ends with [DONE] unless
// Truncated is set. Reset closes the connection after headers are sent.
type Step struct {
	Events    []string
	Delay     time.Duration
	Status    int
	Truncated bool
	Reset     bool
}

// Server is a deterministic OpenAI-compatible streaming test endpoint.
type Server struct {
	HTTP *httptest.Server

	mu       sync.Mutex
	steps    []Step
	requests [][]byte
}

// NewServer starts a scripted SSE endpoint. It calls t.Helper and registers a
// cleanup hook, so callers only need to retain the URL via Server.HTTP.URL.
func NewServer(t testing.TB, steps ...Step) *Server {
	t.Helper()
	s := &Server{steps: append([]Step(nil), steps...)}
	s.HTTP = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.HTTP.Close)
	return s
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.requests = append(s.requests, append([]byte(nil), body...))
	var step Step
	if len(s.steps) > 0 {
		step, s.steps = s.steps[0], s.steps[1:]
	}
	s.mu.Unlock()

	if step.Status != 0 {
		w.WriteHeader(step.Status)
		_, _ = io.WriteString(w, "testkit status fault")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	if step.Reset {
		if h, ok := w.(http.Hijacker); ok {
			conn, _, err := h.Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}
		return
	}
	for _, event := range step.Events {
		if step.Delay > 0 {
			time.Sleep(step.Delay)
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
		if flusher != nil {
			flusher.Flush()
		}
	}
	if !step.Truncated {
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// Requests returns a copy of every request body received so far, in order.
func (s *Server) Requests() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]byte, len(s.requests))
	for i := range s.requests {
		out[i] = append([]byte(nil), s.requests[i]...)
	}
	return out
}

// TextEvent builds a minimal content delta for a scripted answer.
func TextEvent(text string) string {
	return fmt.Sprintf(`{"choices":[{"delta":{"content":%q}}]}`, text)
}

// ToolEvent builds a minimal first tool-call delta.
func ToolEvent(name, args string) string {
	return fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":%q,"arguments":%q}}]}}]}`, name, args)
}
