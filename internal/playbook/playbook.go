// Package playbook implements declarative multi-stage workflows (P8):
// `.yagent/playbooks/<name>.yaml` files that each describe a sequence of
// autonomous goal phases, with per-phase round caps and tool scoping.
package playbook

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Phase is one stage of a playbook: an autonomous goal run with its own tool
// scope and round cap.
type Phase struct {
	// Goal is the autonomous-goal prompt for the phase (DONE/CONTINUE rounds).
	Goal string `yaml:"goal"`
	// Rounds caps the phase (default 8 when 0).
	Rounds int `yaml:"rounds"`
	// Tools scopes the phase to this read/write tool subset (empty = all).
	Tools []string `yaml:"tools"`
	// Success is a human-readable acceptance note.
	Success string `yaml:"success"`
	// Checks are machine-verifiable success predicates (Luna review #11): the
	// model's DONE verdict is a proposal — when checks are set, the phase only
	// completes if they all pass.
	Checks []Check `yaml:"checks"`
}

// HasChecks reports whether the phase has deterministic success predicates.
func (p Phase) HasChecks() bool { return len(p.Checks) > 0 }

// FileAssert is one file-content assertion (same shape as the eval harness).
type FileAssert struct {
	Path string `yaml:"path"`
	Text string `yaml:"text"`
}

// Check is one deterministic success predicate for a phase.
type Check struct {
	FileContains    *FileAssert `yaml:"file_contains"`
	FileNotContains *FileAssert `yaml:"file_not_contains"`
	FileExists      string      `yaml:"file_exists"`
	// DiagnosticsPass requires workspace_diagnostics to report a clean run; the
	// runner evaluates it (it needs the tool runtime).
	DiagnosticsPass bool `yaml:"diagnostics"`
	// TestsPass requires the unit tests to pass (e.g. TDD/refactor phases). It
	// is evaluated by the runner via test_runner; the value is a test filter
	// (symbol name) or empty for the whole suite.
	TestsPass string `yaml:"tests"`
}

// Evaluate runs the file-based predicates against the workspace and returns a
// description of each failure (empty = all passed). The diagnostics predicate
// is evaluated by the caller.
func (c Check) Evaluate(ws string) []string {
	var fails []string
	if c.FileContains != nil {
		data, err := os.ReadFile(filepath.Join(ws, c.FileContains.Path))
		if err != nil || !strings.Contains(string(data), c.FileContains.Text) {
			fails = append(fails, fmt.Sprintf("file %s does not contain %q", c.FileContains.Path, c.FileContains.Text))
		}
	}
	if c.FileNotContains != nil {
		data, err := os.ReadFile(filepath.Join(ws, c.FileNotContains.Path))
		if err == nil && strings.Contains(string(data), c.FileNotContains.Text) {
			fails = append(fails, fmt.Sprintf("file %s contains %q (should not)", c.FileNotContains.Path, c.FileNotContains.Text))
		}
	}
	if c.FileExists != "" {
		if _, err := os.Stat(filepath.Join(ws, c.FileExists)); err != nil {
			fails = append(fails, fmt.Sprintf("file %s does not exist", c.FileExists))
		}
	}
	return fails
}

// Playbook is a declarative multi-stage workflow.
type Playbook struct {
	Name        string  `yaml:"name"`
	Description string  `yaml:"description"`
	Phases      []Phase `yaml:"phases"`
}

// Dir returns the project playbooks directory.
func Dir(ws string) string { return filepath.Join(ws, ".yagent", "playbooks") }

// List returns the available playbook names (the *.yaml basenames), sorted.
func List(ws string) []string {
	entries, err := os.ReadDir(Dir(ws))
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		names = append(names, strings.TrimSuffix(e.Name(), ".yaml"))
	}
	sort.Strings(names)
	return names
}

// Load reads and validates a playbook by name from the workspace.
func Load(ws, name string) (*Playbook, error) {
	if name == "" || strings.ContainsAny(name, `/\`) {
		return nil, fmt.Errorf("invalid playbook name %q", name)
	}
	path := filepath.Join(Dir(ws), name+".yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("playbook %q: %w", name, err)
	}
	var pb Playbook
	if err := yaml.Unmarshal(data, &pb); err != nil {
		return nil, fmt.Errorf("parse playbook %s: %w", path, err)
	}
	if pb.Name == "" {
		pb.Name = name
	}
	if len(pb.Phases) == 0 {
		return nil, fmt.Errorf("playbook %q has no phases", name)
	}
	for i, ph := range pb.Phases {
		if ph.Goal == "" {
			return nil, fmt.Errorf("playbook %q phase %d has no goal", name, i+1)
		}
	}
	return &pb, nil
}
