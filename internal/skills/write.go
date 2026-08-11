package skills

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ValidationError marks an argument/content problem the model can fix and
// retry; tools surface it as a validation failure.
type ValidationError struct{ msg string }

func (e *ValidationError) Error() string { return e.msg }

func vf(format string, args ...any) error {
	return &ValidationError{msg: fmt.Sprintf(format, args...)}
}

// Actions supported by skill_manage.
const (
	ActionCreate     = "create"
	ActionPatch      = "patch"
	ActionEdit       = "edit"
	ActionDelete     = "delete"
	ActionWriteFile  = "write_file"
	ActionRemoveFile = "remove_file"
)

// Op is one skill mutation. scope is "global" (default) or "project".
// Source (default "agent") marks who authored the skill; user-authored skills
// are exempt from the dangerous-pattern scanner.
type Op struct {
	Action      string
	Name        string
	Scope       string
	Source      string
	Content     string // create/edit: full SKILL.md
	Category    string // create: category when not in the frontmatter
	OldString   string // patch
	NewString   string // patch
	FilePath    string // write_file/remove_file
	FileContent string // write_file
}

// pending is the on-disk staged write.
type pending struct {
	ID        string
	CreatedAt int64
	Op        Op
	Diff      string
	Failures  int // failed verification runs against this staged write
}

// PendingSummary is one row of `/skills pending`.
type PendingSummary struct {
	ID        string
	Action    string
	Name      string
	CreatedAt int64
	Failures  int
}

// validate checks an op against the authoring rules, the safety scanner and
// the anti-hoarding dedup. It returns the scanner flag warning (nil when the
// content is clean). Errors are ValidationErrors the model can retry.
func (s *Store) validate(op Op) (string, error) {
	if !slugRe.MatchString(op.Name) {
		return "", vf("invalid name %q: must match [a-z][a-z0-9_-]*", op.Name)
	}
	if op.Scope != "" && op.Scope != "global" && op.Scope != "project" {
		return "", vf("invalid scope %q: use global or project", op.Scope)
	}
	if op.Scope == "" {
		op.Scope = "global"
	}

	switch op.Action {
	case ActionCreate:
		fm, body, err := parseFrontmatter(op.Content)
		if err != nil {
			return "", vf("invalid SKILL.md: %v", err)
		}
		if fm.Name == "" {
			fm.Name = op.Name
		}
		if !slugRe.MatchString(fm.Name) {
			return "", vf("invalid name %q: must match [a-z][a-z0-9_-]*", fm.Name)
		}
		if fm.Name != op.Name {
			return "", vf("frontmatter name %q does not match the requested name %q", fm.Name, op.Name)
		}
		if fm.Category == "" {
			fm.Category = op.Category
		}
		if err := validateFM(fm); err != nil {
			return "", err
		}
		if err := validateBody(body); err != nil {
			return "", err
		}
		if len(op.Content) > MaxSkillBytes {
			return "", vf("SKILL.md is %d bytes; cap is %d", len(op.Content), MaxSkillBytes)
		}
		if s.Exists(op.Name) {
			return "", vf("skill %q already exists; propose a patch to extend it instead of creating a duplicate", op.Name)
		}
		if dup, ok := s.findDuplicate(op.Name, fm.Description, fm.Category); ok {
			return "", vf("skill %q already covers this procedure; propose a patch to it instead of creating a duplicate", dup)
		}
		return s.scanWarning(op, op.Content)

	case ActionEdit:
		if _, _, ok := s.findSkill(op.Name); !ok {
			return "", vf("unknown skill %q", op.Name)
		}
		fm, body, err := parseFrontmatter(op.Content)
		if err != nil {
			return "", vf("invalid SKILL.md: %v", err)
		}
		if fm.Name == "" {
			fm.Name = op.Name
		}
		if fm.Name != op.Name {
			return "", vf("frontmatter name %q does not match the requested name %q", fm.Name, op.Name)
		}
		if err := validateFM(fm); err != nil {
			return "", err
		}
		if err := validateBody(body); err != nil {
			return "", err
		}
		if len(op.Content) > MaxSkillBytes {
			return "", vf("SKILL.md is %d bytes; cap is %d", len(op.Content), MaxSkillBytes)
		}
		return s.scanWarning(op, op.Content)

	case ActionPatch:
		dir, _, ok := s.findSkill(op.Name)
		if !ok {
			return "", vf("unknown skill %q", op.Name)
		}
		if op.OldString == "" {
			return "", vf(`"old_string" is required`)
		}
		current, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
		if err != nil {
			return "", vf("read skill: %v", err)
		}
		cur := string(current)
		n := strings.Count(cur, op.OldString)
		switch n {
		case 0:
			return "", vf("old_string not found in %q; pass the exact text from the current file", op.Name)
		case 1:
			// ok
		default:
			return "", vf("old_string matches %d times in %q; include more surrounding context", n, op.Name)
		}
		patched := strings.Replace(cur, op.OldString, op.NewString, 1)
		fm, _, err := parseFrontmatter(patched)
		if err != nil {
			return "", vf("patched skill invalid: %v", err)
		}
		if fm.Name != op.Name {
			return "", vf("patch would change the skill name to %q; names are immutable", fm.Name)
		}
		if err := validateFM(fm); err != nil {
			return "", err
		}
		if len(patched) > MaxSkillBytes {
			return "", vf("patched SKILL.md is %d bytes; cap is %d", len(patched), MaxSkillBytes)
		}
		return s.scanWarning(op, patched)

	case ActionDelete:
		return "", nil

	case ActionWriteFile:
		dir, _, ok := s.findSkill(op.Name)
		if !ok {
			return "", vf("unknown skill %q", op.Name)
		}
		if op.FilePath == "" {
			return "", vf(`"file_path" is required`)
		}
		if op.FilePath == "SKILL.md" {
			return "", vf("SKILL.md is managed with create/edit; write_file is for reference files")
		}
		if _, err := safeJoin(dir, op.FilePath); err != nil {
			return "", err
		}
		if len(op.FileContent) > MaxRefBytes {
			return "", vf("reference file is %d bytes; cap is %d", len(op.FileContent), MaxRefBytes)
		}
		return s.scanWarning(op, op.FileContent)

	case ActionRemoveFile:
		if op.FilePath == "" {
			return "", vf(`"file_path" is required`)
		}
		if op.FilePath == "SKILL.md" {
			return "", vf("SKILL.md cannot be removed; use the delete action")
		}
		return "", nil

	default:
		return "", vf("unknown action %q: use create, patch, edit, delete, write_file or remove_file", op.Action)
	}
}

// scanWarning turns a scanner verdict into the flag warning text. User-authored
// skills (imported by the user) are exempt: the user wrote them, so the guard
// is not applied.
func (s *Store) scanWarning(op Op, content string) (string, error) {
	if op.Source == SourceUser {
		return "", nil
	}
	v := Scan(content)
	if v.Blocked {
		return "", vf("content blocked by the safety scanner: %s", strings.Join(v.Reasons, "; "))
	}
	if v.Flagged {
		return "content flagged by the safety scanner: " + strings.Join(v.Reasons, "; "), nil
	}
	return "", nil
}

// findDuplicate reports an existing skill covering the same procedure: same
// name (skipped), same category (or any category when the proposal has none),
// and a description with high word overlap (Jaccard >= 0.5).
func (s *Store) findDuplicate(name, description, category string) (string, bool) {
	want := wordSet(description)
	if len(want) == 0 {
		return "", false
	}
	for _, m := range s.List() {
		if m.Name == name {
			continue
		}
		if category != "" && m.Category != category {
			continue
		}
		if overlap(want, wordSet(m.Description)) >= 0.5 {
			return m.Name, true
		}
	}
	return "", false
}

func wordSet(s string) map[string]bool {
	words := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
	set := map[string]bool{}
	for _, w := range words {
		set[w] = true
	}
	return set
}

// overlap is the Jaccard similarity of two word sets (distinct words).
func overlap(a, b map[string]bool) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}
	inter := 0
	for w := range a {
		if b[w] {
			inter++
		}
	}
	union := len(a)
	for w := range b {
		if !a[w] {
			union++
		}
	}
	return float64(inter) / float64(union)
}

// skillTarget resolves where a write lands for a given scope.
func (s *Store) skillTarget(op Op) string {
	root := s.globalRoot()
	if op.Scope == "project" {
		root = s.projectDir
	}
	// category comes from the frontmatter for create; read it here.
	category := op.Category
	if op.Action == ActionCreate && op.Content != "" {
		if fm, _, err := parseFrontmatter(op.Content); err == nil {
			category = fm.Category
			if category == "" {
				category = op.Category
			}
		}
	}
	if category == "" {
		return filepath.Join(root, op.Name)
	}
	return filepath.Join(root, category, op.Name)
}

// Apply validates and performs an op immediately. Used when the approval gate
// is off and by pending approval.
func (s *Store) Apply(op Op) (string, error) {
	warning, err := s.validate(op)
	if err != nil {
		return "", err
	}
	if err := s.apply(op); err != nil {
		return "", err
	}
	return warning, nil
}

func (s *Store) apply(op Op) error {
	switch op.Action {
	case ActionCreate:
		fm, body, err := parseFrontmatter(op.Content)
		if err != nil {
			return err
		}
		if fm.Category == "" {
			fm.Category = op.Category
		}
		// Store-managed fields: never taken from the model.
		if op.Source == "" {
			op.Source = SourceAgent
		}
		fm.Source = op.Source
		fm.CreatedAt = time.Now().Unix()
		fm.LastUsed = time.Now().Unix()
		dir := s.skillTarget(op)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create skill dir: %w", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(renderSkill(fm, body)), 0o644); err != nil {
			return fmt.Errorf("write SKILL.md: %w", err)
		}

	case ActionEdit:
		dir, _, ok := s.findSkill(op.Name)
		if !ok {
			return fmt.Errorf("unknown skill %q", op.Name)
		}
		fm, body, err := parseFrontmatter(op.Content)
		if err != nil {
			return err
		}
		fm.Source = s.preserveSource(op.Name, SourceAgent)
		fm.CreatedAt = s.preserveCreatedAt(op.Name, fm.CreatedAt)
		fm.LastUsed = time.Now().Unix()
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(renderSkill(fm, body)), 0o644); err != nil {
			return fmt.Errorf("write SKILL.md: %w", err)
		}

	case ActionPatch:
		dir, _, ok := s.findSkill(op.Name)
		if !ok {
			return fmt.Errorf("unknown skill %q", op.Name)
		}
		path := filepath.Join(dir, "SKILL.md")
		current, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read skill: %w", err)
		}
		patched := strings.Replace(string(current), op.OldString, op.NewString, 1)
		fm, body, err := parseFrontmatter(patched)
		if err != nil {
			return err
		}
		fm.Source = s.preserveSource(op.Name, SourceAgent)
		fm.CreatedAt = s.preserveCreatedAt(op.Name, fm.CreatedAt)
		fm.LastUsed = time.Now().Unix()
		if err := os.WriteFile(path, []byte(renderSkill(fm, body)), 0o644); err != nil {
			return fmt.Errorf("write SKILL.md: %w", err)
		}

	case ActionDelete:
		dir, _, ok := s.findSkill(op.Name)
		if !ok {
			return fmt.Errorf("unknown skill %q", op.Name)
		}
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("delete skill: %w", err)
		}

	case ActionWriteFile:
		dir, _, ok := s.findSkill(op.Name)
		if !ok {
			return fmt.Errorf("unknown skill %q", op.Name)
		}
		target, err := safeJoin(dir, op.FilePath)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("create reference dir: %w", err)
		}
		if err := os.WriteFile(target, []byte(op.FileContent), 0o644); err != nil {
			return fmt.Errorf("write reference file: %w", err)
		}

	case ActionRemoveFile:
		dir, _, ok := s.findSkill(op.Name)
		if !ok {
			return fmt.Errorf("unknown skill %q", op.Name)
		}
		target, err := safeJoin(dir, op.FilePath)
		if err != nil {
			return err
		}
		if err := os.Remove(target); err != nil {
			return fmt.Errorf("remove reference file: %w", err)
		}

	default:
		return vf("unknown action %q", op.Action)
	}
	return nil
}

// preserveCreatedAt keeps the original creation time of an existing skill.
func (s *Store) preserveCreatedAt(name string, fallback int64) int64 {
	if dir, _, ok := s.findSkill(name); ok {
		if fm, _, err := parseFrontmatter(readFile(filepath.Join(dir, "SKILL.md"))); err == nil && fm.CreatedAt != 0 {
			return fm.CreatedAt
		}
	}
	if fallback != 0 {
		return fallback
	}
	return time.Now().Unix()
}

// preserveSource keeps the original author of an existing skill across edits.
func (s *Store) preserveSource(name, fallback string) string {
	if dir, _, ok := s.findSkill(name); ok {
		if fm, _, err := parseFrontmatter(readFile(filepath.Join(dir, "SKILL.md"))); err == nil && fm.Source != "" {
			return fm.Source
		}
	}
	return fallback
}

// ImportFile reads a SKILL.md file and stores it in scope as a user-authored
// skill (dangerous-pattern scanner exempt). Used by `yagent skills import`.
func (s *Store) ImportFile(path, scope string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return s.ImportContent(string(content), scope)
}

// ImportContent stores SKILL.md content in scope as a user-authored skill.
func (s *Store) ImportContent(content, scope string) (string, error) {
	fm, _, err := parseFrontmatter(content)
	if err != nil {
		return "", err
	}
	if fm.Name == "" {
		return "", vf("SKILL.md has no name in its frontmatter")
	}
	op := Op{Action: ActionCreate, Name: fm.Name, Scope: scope, Source: SourceUser, Content: content}
	return s.Apply(op)
}

// Stage validates an op, computes a review diff and writes it to
// <data>/pending/skills/<id>/ instead of applying (approval gate on).
func (s *Store) Stage(op Op) (id, warning string, err error) {
	warning, err = s.validate(op)
	if err != nil {
		return "", "", err
	}
	diff, err := s.computeDiff(op)
	if err != nil {
		return "", "", err
	}
	id, err = newID()
	if err != nil {
		return "", "", err
	}
	p := pending{ID: id, CreatedAt: time.Now().Unix(), Op: op, Diff: diff}
	dir := filepath.Join(s.pendingRoot(), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", fmt.Errorf("create pending dir: %w", err)
	}
	data, err := json.Marshal(p)
	if err != nil {
		return "", "", fmt.Errorf("marshal pending: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "item.json"), data, 0o644); err != nil {
		return "", "", fmt.Errorf("write pending: %w", err)
	}
	s.stagedWrites++
	return id, warning, nil
}

// computeDiff renders the before/after for review.
func (s *Store) computeDiff(op Op) (string, error) {
	label := op.Name
	var oldText, newText string
	switch op.Action {
	case ActionCreate:
		newText = op.Content
	case ActionEdit:
		if dir, _, ok := s.findSkill(op.Name); ok {
			oldText = readFile(filepath.Join(dir, "SKILL.md"))
		}
		newText = op.Content
	case ActionPatch:
		if dir, _, ok := s.findSkill(op.Name); ok {
			oldText = readFile(filepath.Join(dir, "SKILL.md"))
			newText = strings.Replace(oldText, op.OldString, op.NewString, 1)
		}
	case ActionDelete:
		if dir, _, ok := s.findSkill(op.Name); ok {
			oldText = readFile(filepath.Join(dir, "SKILL.md"))
		}
	case ActionWriteFile:
		label = op.Name + "/" + op.FilePath
		if dir, _, ok := s.findSkill(op.Name); ok {
			if t, err := safeJoin(dir, op.FilePath); err == nil {
				oldText = readFile(t)
			}
		}
		newText = op.FileContent
	case ActionRemoveFile:
		label = op.Name + "/" + op.FilePath
		if dir, _, ok := s.findSkill(op.Name); ok {
			if t, err := safeJoin(dir, op.FilePath); err == nil {
				oldText = readFile(t)
			}
		}
	}
	return lineDiff(label, oldText, newText), nil
}

// ListPending returns staged writes, oldest first.
func (s *Store) ListPending() ([]PendingSummary, error) {
	entries, err := os.ReadDir(s.pendingRoot())
	if err != nil {
		return nil, fmt.Errorf("read pending: %w", err)
	}
	var out []PendingSummary
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p, err := s.loadPending(e.Name())
		if err != nil {
			continue
		}
		out = append(out, PendingSummary{ID: p.ID, Action: p.Op.Action, Name: p.Op.Name, CreatedAt: p.CreatedAt, Failures: p.Failures})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out, nil
}

// RecordPendingFailure increments a staged write's failed-verification count.
func (s *Store) RecordPendingFailure(id string) error {
	p, err := s.loadPending(id)
	if err != nil {
		return err
	}
	p.Failures++
	return s.savePending(p)
}

// ClearPendingFailures resets a staged write's verification failures.
func (s *Store) ClearPendingFailures(id string) error {
	p, err := s.loadPending(id)
	if err != nil {
		return err
	}
	p.Failures = 0
	return s.savePending(p)
}

func (s *Store) savePending(p pending) error {
	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal pending: %w", err)
	}
	return os.WriteFile(filepath.Join(s.pendingRoot(), p.ID, "item.json"), data, 0o644)
}

// PendingDiff returns the review diff for a staged write.
func (s *Store) PendingDiff(id string) (string, error) {
	p, err := s.loadPending(id)
	if err != nil {
		return "", err
	}
	return p.Diff, nil
}

// PendingSkillContent returns the SKILL.md content a staged write would
// produce, for the verification harness. Returns "" for writes that remove a
// skill or change no SKILL.md.
func (s *Store) PendingSkillContent(id string) (string, error) {
	p, err := s.loadPending(id)
	if err != nil {
		return "", err
	}
	switch p.Op.Action {
	case ActionCreate, ActionEdit:
		return p.Op.Content, nil
	case ActionPatch:
		if dir, _, ok := s.findSkill(p.Op.Name); ok {
			cur := readFile(filepath.Join(dir, "SKILL.md"))
			return strings.Replace(cur, p.Op.OldString, p.Op.NewString, 1), nil
		}
	case ActionWriteFile:
		if dir, _, ok := s.findSkill(p.Op.Name); ok {
			return readFile(filepath.Join(dir, "SKILL.md")), nil
		}
	}
	return "", nil
}

// PendingName returns the skill name a staged write targets.
func (s *Store) PendingName(id string) (string, error) {
	p, err := s.loadPending(id)
	if err != nil {
		return "", err
	}
	return p.Op.Name, nil
}

// ApprovePending applies a staged write and removes it from the queue.
func (s *Store) ApprovePending(id string) (string, error) {
	p, err := s.loadPending(id)
	if err != nil {
		return "", err
	}
	warning, err := s.Apply(p.Op)
	if err != nil {
		return "", err
	}
	if err := os.RemoveAll(filepath.Join(s.pendingRoot(), id)); err != nil {
		return "", fmt.Errorf("clear pending: %w", err)
	}
	return warning, nil
}

// RejectPending drops a staged write without applying it.
func (s *Store) RejectPending(id string) error {
	if _, err := s.loadPending(id); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(s.pendingRoot(), id))
}

func (s *Store) loadPending(id string) (pending, error) {
	var p pending
	data, err := os.ReadFile(filepath.Join(s.pendingRoot(), id, "item.json"))
	if err != nil {
		return p, fmt.Errorf("unknown pending write %q", id)
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return p, fmt.Errorf("corrupt pending write %q: %w", id, err)
	}
	return p, nil
}
