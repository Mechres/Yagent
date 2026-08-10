package eval

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestEvals runs every golden task under testdata/evals against the scripted
// fake servers. This is the M6 regression harness for the M2-M5 acceptance
// flows; it needs no network.
func TestEvals(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "evals", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no eval files found: %v", err)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		var task Task
		if err := yaml.Unmarshal(data, &task); err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		t.Run(task.Name, func(t *testing.T) {
			Run(t, task)
		})
	}
}
