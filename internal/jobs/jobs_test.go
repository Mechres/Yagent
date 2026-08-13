package jobs

import (
	"os"
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

func TestKillKillsDescendants(t *testing.T) {
	ws := t.TempDir()
	marker := ws + "/done"
	r := New()
	// "& echo" forces sh to fork a child that writes a marker later; killing
	// the job must take the whole process group down before the marker can
	// appear (regression: only the shell was killed, the child survived).
	job, err := r.Start("sleep 3 & (sleep 2; touch " + marker + ") & wait")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if err := r.Kill(job.ID); err != nil {
		t.Fatal(err)
	}
	if job.Alive() {
		t.Error("job still alive after kill")
	}
	// descendants should be dead too: the marker must not appear
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Error("descendant survived the kill and wrote the marker")
	}
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

func TestStartInSetsWorkingDir(t *testing.T) {
	// Adversarial-QA finding #7 (2026-08-13): a background job previously ran
	// in yagent's process cwd instead of the workspace. StartIn must set the
	// working directory so relative paths resolve against the workspace.
	ws := t.TempDir()
	if err := os.MkdirAll(ws+"/sub", 0o755); err != nil {
		t.Fatal(err)
	}
	r := New()
	job, err := r.StartIn("pwd", ws)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Kill(job.ID)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(job.Logs(1<<20), ws) {
			return // cwd is the workspace
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("job pwd = %q, want workspace %q", job.Logs(1<<20), ws)
}
