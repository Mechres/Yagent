package llm

import (
	"context"
	"sync"
)

// Slot-locked inference (single-slot local servers): a process-wide, per-server
// semaphore so parallel subagents, consult calls, embeddings and tokenizer
// probes never hit a single-slot llama.cpp/Ollama concurrently and get HTTP 500
// ("no slot available") or thrash VRAM. The capacity defaults to 1 and can be
// raised to the server's real slot count (from /props).

var (
	defaultSlots = 1
	slotMu       sync.Mutex
	slotMap      = map[string]*slotLimiter{}
)

// SetDefaultSlotLimit sets the concurrency limit used for servers that haven't
// been explicitly limited. Call before the first request (e.g. after probing
// /props); it takes effect for limiters created afterwards.
func SetDefaultSlotLimit(n int) {
	slotMu.Lock()
	defer slotMu.Unlock()
	if n >= 1 {
		defaultSlots = n
	}
}

type slotLimiter struct {
	sem chan struct{}
}

// limiterFor returns the per-URL limiter, created with the current default
// capacity on first use.
func limiterFor(url string) *slotLimiter {
	slotMu.Lock()
	defer slotMu.Unlock()
	if l, ok := slotMap[url]; ok {
		return l
	}
	l := &slotLimiter{sem: make(chan struct{}, defaultSlots)}
	slotMap[url] = l
	return l
}

// acquire blocks until a slot is free or ctx is done.
func (l *slotLimiter) acquire(ctx context.Context) error {
	select {
	case l.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *slotLimiter) release() {
	<-l.sem
}

// acquireSlot serializes one inference request for the server at url.
func acquireSlot(ctx context.Context, url string) (func(), error) {
	l := limiterFor(url)
	if err := l.acquire(ctx); err != nil {
		return nil, err
	}
	return l.release, nil
}
