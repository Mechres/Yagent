package capsule

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecordMatchAndPersist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "capsules.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	// first failure: recorded, no hint yet (single failure is normal)
	c := s.Record("fs_edit", "old_string_not_found", "main.go")
	if c.Failures != 1 {
		t.Errorf("failures = %d, want 1", c.Failures)
	}
	if h := Hint(c); h != "" {
		t.Errorf("first failure should not hint: %q", h)
	}

	// second failure: hint fires, naming the recurring pattern
	c = s.Record("fs_edit", "old_string_not_found", "main.go")
	if c.Failures != 2 {
		t.Errorf("failures = %d, want 2", c.Failures)
	}
	h := Hint(c)
	if h == "" || !strings.Contains(h, "recurring failure") || !strings.Contains(h, "fs_edit") {
		t.Errorf("hint = %q", h)
	}

	// Match resolves the same triple
	m, ok := s.Match("fs_edit", "old_string_not_found", "main.go")
	if !ok || m.Failures != 2 {
		t.Errorf("match = %+v ok=%v", m, ok)
	}

	// recovery: fs_write succeeds on main.go afterwards -> the capsule learns it
	s.RecordRecovery("main.go", "fs_write")
	m, _ = s.Match("fs_edit", "old_string_not_found", "main.go")
	if m.RecoveredBy != "fs_write" {
		t.Errorf("recovered_by = %q, want fs_write", m.RecoveredBy)
	}
	h2 := Hint(m)
	if !strings.Contains(h2, "fs_write") {
		t.Errorf("recovery hint = %q", h2)
	}

	// persistence: reload sees the same capsule
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	m2, ok := s2.Match("fs_edit", "old_string_not_found", "main.go")
	if !ok || m2.RecoveredBy != "fs_write" || m2.Failures != 2 {
		t.Errorf("reloaded = %+v ok=%v", m2, ok)
	}
}

func TestMatchFallbacks(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "c.json"))
	if err != nil {
		t.Fatal(err)
	}
	// (tool, class) on one path falls back to any path
	s.Record("fs_edit", "old_string_not_found", "a.go")
	m, ok := s.Match("fs_edit", "old_string_not_found", "b.go")
	if !ok || m.Path != "a.go" {
		t.Errorf("class fallback = %+v ok=%v", m, ok)
	}
	// unknown class -> no match
	if _, ok := s.Match("fs_edit", "unknown_class", "b.go"); ok {
		t.Error("unknown class should not match")
	}
}

func TestErrClassOf(t *testing.T) {
	if got := ErrClassOf("error: old_string not found [class=old_string_not_found retryable=true suggest=fs_read]"); got != "old_string_not_found" {
		t.Errorf("ErrClassOf = %q", got)
	}
	if got := ErrClassOf("error: plain message"); got != "" {
		t.Errorf("ErrClassOf(plain) = %q, want empty", got)
	}
}

func TestCorruptStoreOpensEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.json")
	_ = os.WriteFile(path, []byte("{not json"), 0o600)
	s, err := Open(path)
	if err != nil {
		t.Fatalf("corrupt store should open as empty, got error: %v", err)
	}
	if got := len(s.List()); got != 0 {
		t.Errorf("corrupt store list = %d, want 0", got)
	}
}
