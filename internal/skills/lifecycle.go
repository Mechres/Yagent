package skills

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	ActionPin     = "pin"
	ActionUnpin   = "unpin"
	ActionArchive = "archive"
	ActionRestore = "restore"
)

// SnapshotMeta identifies a recoverable pre-mutation copy.
type SnapshotMeta struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Action    string `json:"action"`
	CreatedAt int64  `json:"created_at"`
	Path      string `json:"path"`
}

func (s *Store) snapshotRoot() string { return filepath.Join(s.dataDir, "snapshots", "skills") }
func (s *Store) archiveRoot(project bool) string {
	if project {
		return filepath.Join(filepath.Dir(s.projectDir), "skill-archive")
	}
	return filepath.Join(s.dataDir, "skill-archive")
}

// SetPinned marks a skill as protected from future automated lifecycle work.
func (s *Store) SetPinned(name string, pinned bool) error {
	dir, _, ok := s.findSkill(name)
	if !ok {
		return fmt.Errorf("unknown skill %q", name)
	}
	if _, err := s.CreateSnapshot(name, "pin"); err != nil {
		return err
	}
	fm, body, err := s.readFrontmatter(dir)
	if err != nil {
		return err
	}
	fm.Pinned = pinned
	return os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(renderSkill(fm, body)), 0o644)
}

// CreateSnapshot copies a skill into the recoverable snapshot store and logs
// the mutation. It never changes the live skill.
func (s *Store) CreateSnapshot(name, action string) (SnapshotMeta, error) {
	dir, root, ok := s.findSkill(name)
	if !ok {
		return SnapshotMeta{}, fmt.Errorf("unknown skill %q", name)
	}
	id, err := newID()
	if err != nil {
		return SnapshotMeta{}, err
	}
	dst := filepath.Join(s.snapshotRoot(), id)
	if err := copyDir(dir, dst); err != nil {
		return SnapshotMeta{}, err
	}
	m := SnapshotMeta{ID: id, Name: name, Action: action, CreatedAt: time.Now().Unix(), Path: filepath.ToSlash(filepath.Join(root, name))}
	if err := s.appendAudit(m); err != nil {
		return SnapshotMeta{}, err
	}
	return m, nil
}

// ListSnapshots returns the recoverable history, newest first.
func (s *Store) ListSnapshots(name string) ([]SnapshotMeta, error) {
	data, err := os.ReadFile(filepath.Join(s.dataDir, "snapshots", "skills-audit.jsonl"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []SnapshotMeta
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var m SnapshotMeta
		if json.Unmarshal([]byte(line), &m) == nil && (name == "" || m.Name == name) {
			if _, statErr := os.Stat(filepath.Join(s.snapshotRoot(), m.ID)); statErr == nil {
				out = append(out, m)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out, nil
}

// Archive moves a skill out of the active roots after taking a snapshot.
func (s *Store) Archive(name string) error {
	dir, root, ok := s.findSkill(name)
	if !ok {
		return fmt.Errorf("unknown skill %q", name)
	}
	if _, err := s.CreateSnapshot(name, "archive"); err != nil {
		return err
	}
	project := root == RootProject
	dst := filepath.Join(s.archiveRoot(project), name)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("archived skill %q already exists", name)
	}
	return os.Rename(dir, dst)
}

// Restore brings an archived skill back to its original scope.
func (s *Store) Restore(name string) error {
	if s.Exists(name) {
		return fmt.Errorf("skill %q already exists", name)
	}
	for _, project := range []bool{true, false} {
		src := filepath.Join(s.archiveRoot(project), name)
		if _, err := os.Stat(filepath.Join(src, "SKILL.md")); err != nil {
			continue
		}
		dst := filepath.Join(s.skillRootFor(project), name)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.Rename(src, dst)
	}
	return fmt.Errorf("no archived skill %q", name)
}

func (s *Store) skillRootFor(project bool) string {
	if project {
		return s.projectDir
	}
	return s.globalRoot()
}

func (s *Store) appendAudit(m SnapshotMeta) error {
	path := filepath.Join(s.dataDir, "snapshots", "skills-audit.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, _ := json.Marshal(m)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, string(b))
	return err
}

func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		_, cpErr := io.Copy(out, in)
		closeErr := out.Close()
		if cpErr != nil {
			return cpErr
		}
		return closeErr
	})
}
