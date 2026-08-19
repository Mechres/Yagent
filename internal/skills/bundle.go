package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	maxBundleSkills      = 8
	maxBundleInstruction = 2000
)

var bundleNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// Bundle is a local alias for existing skills plus one short instruction.
// Bundles never contain skill bodies and never fetch remote content.
type Bundle struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Skills      []string `yaml:"skills"`
	Instruction string   `yaml:"instruction"`
}

func (s *Store) bundleRoots() []string {
	return []string{filepath.Join(filepath.Dir(s.projectDir), "bundles"), filepath.Join(s.dataDir, "bundles")}
}

// ListBundles returns visible bundle names in project-over-global order.
func (s *Store) ListBundles() []string {
	seen := map[string]bool{}
	var out []string
	for _, root := range s.bundleRoots() {
		entries, _ := os.ReadDir(root)
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
				continue
			}
			name := strings.TrimSuffix(e.Name(), ".yaml")
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)
	return out
}

// LoadBundle loads a validated local bundle, with project bundles shadowing
// global bundles of the same name.
func (s *Store) LoadBundle(name string) (Bundle, error) {
	if !bundleNameRE.MatchString(name) {
		return Bundle{}, fmt.Errorf("invalid bundle name %q", name)
	}
	for _, root := range s.bundleRoots() {
		path := filepath.Join(root, name+".yaml")
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return Bundle{}, fmt.Errorf("read bundle: %w", err)
		}
		var b Bundle
		if err := yaml.Unmarshal(data, &b); err != nil {
			return Bundle{}, fmt.Errorf("parse bundle: %w", err)
		}
		if b.Name == "" {
			b.Name = name
		}
		if b.Name != name || !bundleNameRE.MatchString(b.Name) {
			return Bundle{}, fmt.Errorf("bundle name must match %q", name)
		}
		if len(b.Skills) == 0 || len(b.Skills) > maxBundleSkills {
			return Bundle{}, fmt.Errorf("bundle must contain 1-%d skills", maxBundleSkills)
		}
		if len(b.Instruction) > maxBundleInstruction {
			return Bundle{}, fmt.Errorf("bundle instruction exceeds %d characters", maxBundleInstruction)
		}
		if Scan(b.Instruction).Blocked {
			return Bundle{}, fmt.Errorf("bundle instruction blocked by safety scanner")
		}
		return b, nil
	}
	return Bundle{}, fmt.Errorf("unknown bundle %q", name)
}
