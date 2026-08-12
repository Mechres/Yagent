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
	// Success is a human-readable acceptance note; the goal loop's DONE verdict
	// is what actually ends the phase.
	Success string `yaml:"success"`
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
