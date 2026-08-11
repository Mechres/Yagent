// Package jobs manages background processes started by shell_bg, with
// accumulated-log access (shell_logs) and termination (shell_kill). It is
// scoped to one chat session; StopAll kills everything on session end.
package jobs

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"sort"
	"sync"
	"syscall"
	"time"
)

// Job is one running background process.
type Job struct {
	ID      string
	Command string
	Start   time.Time

	logMu  sync.Mutex
	log    bytes.Buffer
	proc   *exec.Cmd
	cancel context.CancelFunc
	done   chan struct{}
}

func (j *Job) write(p []byte) (int, error) {
	j.logMu.Lock()
	defer j.logMu.Unlock()
	return j.log.Write(p)
}

// Logs returns the accumulated output, capped at maxBytes (tail).
func (j *Job) Logs(maxBytes int) string {
	j.logMu.Lock()
	defer j.logMu.Unlock()
	s := j.log.String()
	if len(s) > maxBytes {
		return s[len(s)-maxBytes:] + "\n... (truncated)"
	}
	return s
}

// Alive reports whether the process has not exited yet. The done channel is
// closed by the reap goroutine after Wait returns, so this never touches
// ProcessState (which Wait writes) — safe to call from any goroutine.
func (j *Job) Alive() bool {
	select {
	case <-j.done:
		return false
	default:
		return true
	}
}

// Registry holds the session's background jobs.
type Registry struct {
	mu   sync.Mutex
	jobs map[string]*Job
}

// New returns an empty registry.
func New() *Registry { return &Registry{jobs: map[string]*Job{}} }

// Start launches a command via sh -c in the background.
func (r *Registry) Start(command string) (*Job, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	j := &Job{ID: id, Command: command, Start: time.Now(), cancel: cancel, done: make(chan struct{})}
	j.proc = exec.CommandContext(ctx, "sh", "-c", command)
	j.proc.Stdout = writer(j.write)
	j.proc.Stderr = writer(j.write)
	// Own process group so Kill can terminate descendants too (CommandContext
	// only SIGKILLs the shell; a grandchild like `sleep 5` would survive and
	// keep the log pipe open).
	j.proc.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := j.proc.Start(); err != nil {
		cancel()
		return nil, err
	}
	r.mu.Lock()
	r.jobs[j.ID] = j
	r.mu.Unlock()
	go func() { j.proc.Wait(); close(j.done) }() // reap
	return j, nil
}

// Get returns a job by id.
func (r *Registry) Get(id string) (*Job, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[id]
	return j, ok
}

// List returns all jobs, newest first.
func (r *Registry) List() []*Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Job, 0, len(r.jobs))
	for _, j := range r.jobs {
		out = append(out, j)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Start.After(out[k].Start) })
	return out
}

// Kill terminates a job by id, killing the whole process group so descendant
// processes (backgrounded commands) die too.
func (r *Registry) Kill(id string) error {
	j, ok := r.Get(id)
	if !ok {
		return fmt.Errorf("unknown job %q", id)
	}
	killGroup(j)
	<-j.done
	return nil
}

// StopAll terminates every job (session end).
func (r *Registry) StopAll() {
	r.mu.Lock()
	jobs := make([]*Job, 0, len(r.jobs))
	for _, j := range r.jobs {
		jobs = append(jobs, j)
	}
	r.mu.Unlock()
	for _, j := range jobs {
		killGroup(j)
		<-j.done
	}
}

// killGroup SIGKILLs a job's process group (negative pid). Killing an already
// dead group returns ESRCH, which is ignored; Process is read-only after
// Start returns, so this is safe to call concurrently with the reap goroutine.
func killGroup(j *Job) {
	if j.proc.Process != nil {
		_ = syscall.Kill(-j.proc.Process.Pid, syscall.SIGKILL)
	}
	j.cancel()
}

type writer func([]byte) (int, error)

func (w writer) Write(p []byte) (int, error) { return w(p) }

func newID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
