// Package skills implements procedural memory (M3.5): a filesystem store of
// agentskills.io-compatible SKILL.md files under a global data dir plus a
// project store under <workspace>/.yagent/skills, lifecycle metadata,
// progressive disclosure, a dangerous-pattern scanner, write approval staging
// and an anti-hoarding guard. See docs/design/skills.md.
package skills

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Source values for SkillMeta.
const (
	SourceAgent   = "agent"
	SourceUser    = "user"
	SourceBundled = "bundled"
)

// Root labels for SkillMeta.
const (
	RootGlobal  = "global"
	RootProject = "project"
)

// Anti-hoarding guard: at most this many staged skill writes per session.
const MaxStagedPerSession = 2

// MaxSkillFailures is how many failed verifications mark a skill stale at L0.
const MaxSkillFailures = 2

// SkillMeta is the L0 listing entry shown in the system prompt.
type SkillMeta struct {
	Name        string
	Description string
	Category    string
	Source      string
	Root        string // "global" | "project"
	CreatedAt   int64
	LastUsed    int64
	Failures    int // failed verifications; >= MaxSkillFailures marks stale
}

// Store is a filesystem store with two read roots. The project store
// shadows the global store on name collision.
type Store struct {
	dataDir      string
	projectDir   string
	stagedWrites int
}

// Open opens the skills store rooted at dataDir, with the project store at
// workspace/.yagent/skills.
func Open(dataDir, workspace string) (*Store, error) {
	return OpenProject(dataDir, filepath.Join(workspace, ".yagent", "skills"))
}

// OpenProject is Open with an explicit project-store path.
func OpenProject(dataDir, projectDir string) (*Store, error) {
	s := &Store{dataDir: filepath.Clean(dataDir), projectDir: filepath.Clean(projectDir)}
	for _, d := range []string{s.globalRoot(), s.pendingRoot()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("create skills dir %s: %w", d, err)
		}
	}
	return s, nil
}

func (s *Store) globalRoot() string  { return filepath.Join(s.dataDir, "skills") }
func (s *Store) pendingRoot() string { return filepath.Join(s.dataDir, "pending", "skills") }

// Dir returns the store's data directory.
func (s *Store) Dir() string { return s.dataDir }

// readRoots lists the roots in shadow order (project first).
func (s *Store) readRoots() []string {
	return []string{s.projectDir, s.globalRoot()}
}

// StagedCount reports how many skill writes were staged in this session
// (process lifetime). Powers the per-session anti-hoarding cap.
func (s *Store) StagedCount() int { return s.stagedWrites }

// walkSkills maps skill name → skill directory for one root. Any directory
// containing a SKILL.md is a skill; category dirs are just parents.
func walkSkills(root string) map[string]string {
	out := map[string]string{}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if d.Name() != "SKILL.md" {
			return nil
		}
		dir := filepath.Dir(path)
		out[filepath.Base(dir)] = dir
		return nil
	})
	return out
}

// findSkill returns the directory of a skill and its root label.
func (s *Store) findSkill(name string) (dir, root string, ok bool) {
	for _, root := range s.readRoots() {
		if d, found := walkSkills(root)[name]; found {
			return d, s.rootLabel(root), true
		}
	}
	return "", "", false
}

// rootLabel maps a root path to its label.
func (s *Store) rootLabel(root string) string {
	if filepath.Clean(root) == filepath.Clean(s.projectDir) {
		return RootProject
	}
	return RootGlobal
}

// categoryOf returns the immediate parent dir name when it is not the root.
func categoryOf(root, dir string) string {
	parent := filepath.Dir(dir)
	if filepath.Clean(parent) == filepath.Clean(root) {
		return ""
	}
	return filepath.Base(parent)
}

// List returns all skills across both roots, project-first with global
// shadowing, ordered by last_used desc then name (L0 eviction order).
func (s *Store) List() []SkillMeta {
	seen := map[string]bool{}
	var metas []SkillMeta
	for _, root := range s.readRoots() {
		for name, dir := range walkSkills(root) {
			if seen[name] {
				continue
			}
			seen[name] = true
			metas = append(metas, s.readMeta(name, dir, root))
		}
	}
	sort.SliceStable(metas, func(i, j int) bool {
		if metas[i].LastUsed != metas[j].LastUsed {
			return metas[i].LastUsed > metas[j].LastUsed
		}
		return metas[i].Name < metas[j].Name
	})
	return metas
}

func (s *Store) readMeta(name, dir, root string) SkillMeta {
	m := SkillMeta{Name: name, Category: categoryOf(root, dir), Root: s.rootLabel(root)}
	fm, _, err := parseFrontmatter(readFile(filepath.Join(dir, "SKILL.md")))
	if err != nil {
		m.Description = "(invalid SKILL.md — needs repair)"
		m.Source = SourceUser
		return m
	}
	m.Description = fm.Description
	m.Source = fm.Source
	if m.Source == "" {
		m.Source = SourceUser
	}
	m.CreatedAt = fm.CreatedAt
	m.LastUsed = fm.LastUsed
	m.Failures = fm.Failures
	return m
}

// RecordFailure increments a skill's failure counter (a verification run
// failed); at MaxSkillFailures it is shown as stale in the L0 index.
func (s *Store) RecordFailure(name string) error {
	dir, _, ok := s.findSkill(name)
	if !ok {
		return fmt.Errorf("unknown skill %q", name)
	}
	fm, body, err := s.readFrontmatter(dir)
	if err != nil {
		return err
	}
	return writeFrontmatter(dir, fm, body, fm.Failures+1)
}

// ClearFailures resets a skill's failure counter (a verification run passed).
func (s *Store) ClearFailures(name string) error {
	dir, _, ok := s.findSkill(name)
	if !ok {
		return fmt.Errorf("unknown skill %q", name)
	}
	fm, body, err := s.readFrontmatter(dir)
	if err != nil {
		return err
	}
	return writeFrontmatter(dir, fm, body, 0)
}

func (s *Store) readFrontmatter(dir string) (frontmatter, string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return frontmatter{}, "", fmt.Errorf("read skill: %w", err)
	}
	fm, body, err := parseFrontmatter(string(data))
	if err != nil {
		return frontmatter{}, "", fmt.Errorf("parse skill: %w", err)
	}
	return fm, body, nil
}

func writeFrontmatter(dir string, fm frontmatter, body string, failures int) error {
	fm.Failures = failures
	return os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(renderSkill(fm, body)), 0o644)
}

// Exists reports whether a skill with this name is present in any root.
func (s *Store) Exists(name string) bool {
	_, _, ok := s.findSkill(name)
	return ok
}

// View returns a skill's SKILL.md (path == "") or a reference file, plus a
// one-line safety warning when the scanner flags its content. Every view
// bumps last_used for L0 eviction.
func (s *Store) View(name, relPath string) (content, warning string, err error) {
	dir, _, ok := s.findSkill(name)
	if !ok {
		return "", "", fmt.Errorf("unknown skill %q (see skills_list)", name)
	}
	target := filepath.Join(dir, "SKILL.md")
	if relPath != "" {
		target, err = safeJoin(dir, relPath)
		if err != nil {
			return "", "", err
		}
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return "", "", fmt.Errorf("read %s: %w", displayPath(name, relPath), err)
	}
	content = string(data)
	if v := Scan(content); v.Flagged {
		warning = "warning: this skill contains potentially unsafe patterns: " + strings.Join(v.Reasons, "; ")
	}
	if err := s.touchLastUsed(dir); err != nil {
		return "", "", err
	}
	return content, warning, nil
}

// touchLastUsed rewrites SKILL.md with a fresh last_used timestamp.
func (s *Store) touchLastUsed(dir string) error {
	path := filepath.Join(dir, "SKILL.md")
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read skill for last_used: %w", err)
	}
	fm, body, err := parseFrontmatter(string(data))
	if err != nil {
		return nil // unparsable skill: leave it alone
	}
	fm.LastUsed = time.Now().Unix()
	return os.WriteFile(path, []byte(renderSkill(fm, body)), 0o644)
}

// safeJoin resolves rel inside base, rejecting any path that escapes it.
func safeJoin(base, rel string) (string, error) {
	if rel == "" {
		return base, nil
	}
	if filepath.IsAbs(rel) {
		return "", vf("path %q must be relative to the skill directory", rel)
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", vf("path %q escapes the skill directory", rel)
	}
	joined := filepath.Join(base, clean)
	root := filepath.Clean(base)
	if joined != root && !strings.HasPrefix(joined, root+string(filepath.Separator)) {
		return "", vf("path %q escapes the skill directory", rel)
	}
	return joined, nil
}

func displayPath(name, rel string) string {
	if rel == "" {
		return name + "/SKILL.md"
	}
	return name + "/" + filepath.ToSlash(rel)
}

func readFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("random id: %w", err)
	}
	return hex.EncodeToString(b), nil
}
