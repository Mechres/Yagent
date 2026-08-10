package llm

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParseSSE(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatalf("server does not support http.Flusher")
		}
		// send two events then [DONE]
		_, _ = w.Write([]byte("data: hello\n\n"))
		flusher.Flush()
		time.Sleep(10 * time.Millisecond)
		_, _ = w.Write([]byte("data: world\n\n"))
		flusher.Flush()
		time.Sleep(10 * time.Millisecond)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer ts.Close()

	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatalf("http get: %v", err)
	}
	defer resp.Body.Close()

	var events []string
	if err := ParseSSE(resp.Body, func(data string) error {
		events = append(events, data)
		return nil
	}); err != nil {
		t.Fatalf("parse sse: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0] != "hello" || events[1] != "world" {
		t.Fatalf("unexpected events: %v", events)
	}
}
