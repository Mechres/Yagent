package llm

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestSlotLockSerializesInference verifies that concurrent requests to the same
// server URL never overlap when the limit is 1 (the single-slot case).
func TestSlotLockSerializesInference(t *testing.T) {
	SetDefaultSlotLimit(1)
	var active, maxActive int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&active, 1)
		for {
			cur := atomic.LoadInt64(&maxActive)
			if n <= cur || atomic.CompareAndSwapInt64(&maxActive, cur, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt64(&active, -1)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"content":"x"}}]}`)
		flusher.Flush()
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "m")
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := client.ChatStream(context.Background(),
				[]Message{{Role: "user", Content: "x"}}, nil, func(string) {}, nil)
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := atomic.LoadInt64(&maxActive); got > 1 {
		t.Errorf("max concurrent requests = %d, want 1 (serialized)", got)
	}
}

// TestSlotLockAllowsCapacity verifies the limit can be raised (multi-slot server).
func TestSlotLockAllowsCapacity(t *testing.T) {
	SetDefaultSlotLimit(3)
	var active, maxActive int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&active, 1)
		for {
			cur := atomic.LoadInt64(&maxActive)
			if n <= cur || atomic.CompareAndSwapInt64(&maxActive, cur, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt64(&active, -1)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"content":"x"}}]}`)
		flusher.Flush()
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "m")
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = client.ChatStream(context.Background(),
				[]Message{{Role: "user", Content: "x"}}, nil, func(string) {}, nil)
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt64(&maxActive); got < 2 {
		t.Errorf("max concurrent = %d, want >= 2 with limit 3", got)
	}
}

// TestSlotLockCancellation verifies a cancelled waiter releases the slot.
func TestSlotLockCancellation(t *testing.T) {
	SetDefaultSlotLimit(1)
	var block sync.WaitGroup
	block.Add(1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		block.Wait() // hold the slot until released
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"content":"x"}}]}`)
		flusher.Flush()
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "m")
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		_, _ = client.ChatStream(context.Background(), []Message{{Role: "user", Content: "x"}}, nil, func(string) {}, nil)
	}()
	time.Sleep(50 * time.Millisecond) // let the first request grab the slot

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := client.ChatStream(ctx, []Message{{Role: "user", Content: "y"}}, nil, func(string) {}, nil)
	if err == nil {
		t.Error("cancelled waiter should have errored")
	}
	if time.Since(start) > time.Second {
		t.Error("cancellation took too long")
	}
	block.Done()
	<-firstDone
}
