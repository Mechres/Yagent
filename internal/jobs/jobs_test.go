package jobs

import (
	"strings"
	"testing"
	"time"
)

func TestJobLifecycle(t *testing.T) {
	r := New()
	job, err := r.Start("echo hello-bg; sleep 5")
	if err != nil {
		t.Fatal(err)
	}
	// give the process a moment to write
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(job.Logs(1<<20), "hello-bg") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(job.Logs(1<<20), "hello-bg") {
		t.Fatalf("job logs = %q", job.Logs(1<<20))
	}
	if !job.Alive() {
		t.Fatal("job should still be alive (sleep 5)")
	}
	if err := r.Kill(job.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.Kill("nope"); err == nil {
		t.Error("killing unknown job should error")
	}
	r.StopAll()
}

func TestJobsList(t *testing.T) {
	r := New()
	if _, err := r.Start("sleep 5"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Start("sleep 5"); err != nil {
		t.Fatal(err)
	}
	if len(r.List()) != 2 {
		t.Errorf("list = %d jobs", len(r.List()))
	}
	r.StopAll()
}
