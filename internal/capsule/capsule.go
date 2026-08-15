// Package capsule implements persistent "failure capsules": compact,
// project-scoped records of recurring tool failures (tool + normalized error +
// affected path + the tool that eventually recovered). They are written on
// every failed write and on the eventual successful write to the same path,
// then injected into the error feedback when the same failure recurs — even
// across sessions — so a small model stops re-learning the same recovery.
package capsule

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Capsule is one normalized failure record.
type Capsule struct {
	Tool     string `json:"tool"`
	ErrClass string `json:"err_class"`
	Path     string `json:"path"`
	// RecoveredBy is the tool that eventually succeeded on this path after the
	// failures ("" while still un-recovered).
	RecoveredBy string    `json:"recovered_by,omitempty"`
	Failures    int       `json:"failures"`
	LastSeen    time.Time `json:"last_seen"`
}

// Key normalizes a (tool, errClass, path) triple for exact matching.
func Key(tool, errClass, path string) string {
	return strings.ToLower(tool) + "|" + strings.ToLower(errClass) + "|" + strings.ToLower(path)
}

// Store is a JSON-backed capsule store. Not thread-safe across processes;
// within a process it is mutex-guarded.
type Store struct {
	mu   sync.Mutex
	path string
	caps map[string]Capsule
}

// Open loads (or creates) the capsule store at path. A missing or corrupt file
// yields an empty store (a broken capsules file must not disable the session).
func Open(path string) (*Store, error) {
	s := &Store{path: path, caps: map[string]Capsule{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return s, err
	}
	var list []Capsule
	if err := json.Unmarshal(data, &list); err != nil {
		return s, nil // corrupt: start fresh rather than fail the session
	}
	for _, c := range list {
		s.caps[Key(c.Tool, c.ErrClass, c.Path)] = c
	}
	return s, nil
}

// Record notes a failed tool call: bumps the failure count for the normalized
// signature and persists. Returns the capsule (with any prior recovery info)
// so the caller can inject a matching hint.
func (s *Store) Record(tool, errClass, path string) Capsule {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := Key(tool, errClass, path)
	c, ok := s.caps[key]
	if !ok {
		c = Capsule{Tool: tool, ErrClass: errClass, Path: path}
	}
	c.Failures++
	c.LastSeen = time.Now()
	s.caps[key] = c
	s.persistLocked()
	return c
}

// RecordRecovery notes that `tool` eventually succeeded on path after earlier
// failures, and stores it as the recovery path (only the first recovery wins;
// a different successful tool later is kept too via Failures/LastSeen).
func (s *Store) RecordRecovery(path, tool string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// find any un-recovered capsule for this path and mark it
	changed := false
	for key, c := range s.caps {
		if strings.EqualFold(c.Path, path) && c.RecoveredBy == "" {
			c.RecoveredBy = tool
			s.caps[key] = c
			changed = true
		}
	}
	if changed {
		s.persistLocked()
	}
}

// Match returns the most relevant capsule for a (tool, errClass, path) triple,
// or (zero, false) when none exists. Exact (tool, class, path) wins; then
// (tool, class) on any path; then (class, path).
func (s *Store) Match(tool, errClass, path string) (Capsule, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.caps[Key(tool, errClass, path)]; ok {
		return c, true
	}
	// (tool, class) on any path, most recent
	var best Capsule
	found := false
	for key, c := range s.caps {
		if strings.HasPrefix(key, strings.ToLower(tool)+"|"+strings.ToLower(errClass)+"|") &&
			(!found || c.LastSeen.After(best.LastSeen)) {
			best, found = c, true
		}
	}
	if found {
		return best, true
	}
	if c, ok := s.caps[Key(tool, errClass, "")]; ok {
		return c, true
	}
	return Capsule{}, false
}

// ErrClassOf extracts the [class=…] marker from an error result, if present.
func ErrClassOf(result string) string {
	i := strings.Index(result, "[class=")
	if i < 0 {
		return ""
	}
	rest := result[i+len("[class="):]
	j := strings.Index(rest, " ")
	if j < 0 {
		j = strings.Index(rest, "]")
	}
	if j < 0 {
		j = len(rest)
	}
	return rest[:j]
}

// Hint renders a capsule as an injectable nudge appended to a failing tool
// result, or "" when the capsule carries no actionable recovery. Fires only on
// a recurring failure (Failures >= 2) — a single failure is normal, not a loop.
func Hint(c Capsule) string {
	if c.Failures < 2 {
		return ""
	}
	switch c.RecoveredBy {
	case "":
		// known recurring failure with no recovery yet: tell the model it is a
		// known failure pattern so it stops blindly repeating.
		return fmt.Sprintf(" [known recurring failure: this %s error on %q has failed %d time(s); read the exact current content with fs_read and retry, or use a different approach]", c.Tool, c.Path, c.Failures)
	default:
		if c.RecoveredBy == c.Tool {
			return fmt.Sprintf(" [known recurring failure: this %s error on %q has failed %d time(s); in a previous session the eventual successful retry still used %s after reading the exact content]", c.Tool, c.Path, c.Failures, c.RecoveredBy)
		}
		return fmt.Sprintf(" [known recurring failure: this %s error on %q has failed %d time(s); in a previous session %s recovered the situation on this path — consider that tool]", c.Tool, c.Path, c.Failures, c.RecoveredBy)
	}
}

// List returns all capsules, sorted by last-seen (newest first).
func (s *Store) List() []Capsule {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Capsule, 0, len(s.caps))
	for _, c := range s.caps {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	return out
}

func (s *Store) persistLocked() {
	if s.path == "" {
		return
	}
	list := make([]Capsule, 0, len(s.caps))
	for _, c := range s.caps {
		list = append(list, c)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].KeyLast() < list[j].KeyLast() })
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(s.path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	_ = os.WriteFile(s.path, data, 0o600)
}

// KeyLast is a stable sort key for persistence (deterministic file output).
func (c Capsule) KeyLast() string {
	return Key(c.Tool, c.ErrClass, c.Path) + "|" + c.LastSeen.Format(time.RFC3339Nano)
}
