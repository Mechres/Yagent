// Package gitops gives the agent a durable undo/checkpoint layer backed by git
// (borrowed from aider's git integration): instead of an in-memory write buffer
// that a crash wipes, each turn's file changes become a real commit, so /undo
// is a revert and a crashed session can still be rolled back. It also commits
// the user's pre-existing dirty files BEFORE the agent edits, so agent work is
// always separable from user work.
//
// The package deliberately mutates git (commits/reverts), so it is only wired
// when the user opts in (git.auto_commit, default on in a git repo).
package gitops

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// Marker identifies commits made by the agent, so AgentCommits/RevertN can
// find them without trusting a branch name. Prefixes both the message and the
// author trailer.
const Marker = "yagent"

// Commit is one agent-authored commit.
type Commit struct {
	Hash    string
	Subject string
}

// IsRepo reports whether ws is inside a git work tree.
func IsRepo(ws string) bool {
	_, err := run(ws, "rev-parse", "--is-inside-work-tree")
	return err == nil
}

// Dirty reports whether the work tree has uncommitted changes (tracked
// modifications or untracked files).
func Dirty(ws string) bool {
	out, err := run(ws, "status", "--porcelain")
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) != ""
}

// ChangedFiles returns the files git sees as changed/untracked.
func ChangedFiles(ws string) []string {
	out, err := run(ws, "status", "--porcelain")
	if err != nil {
		return nil
	}
	var files []string
	for _, ln := range strings.Split(out, "\n") {
		if len(ln) < 4 {
			continue
		}
		// porcelain: "XY <path>" — path starts at index 3; untracked is "??".
		path := ln[3:]
		if strings.HasPrefix(path, `"`) {
			path = strings.Trim(path, `"`)
		}
		if path != "" {
			files = append(files, path)
		}
	}
	return files
}

// CommitDirty commits everything currently uncommitted with the given subject
// and an "(yagent)" committer trailer, so a pre-existing dirty state is never
// lost or mixed with later agent edits. Returns "" and no error when the tree
// is already clean. Skips when user.name/user.email are unset (the agent must
// not guess identity).
func CommitDirty(ws, subject string) (string, error) {
	return commit(ws, subject, true)
}

// AgentCommit commits the current changes as agent work with the marker
// prefix. Returns "" and no error when nothing changed.
func AgentCommit(ws, subject string) (string, error) {
	return commit(ws, Marker+": "+subject, false)
}

func commit(ws, subject string, dirty bool) (string, error) {
	if !IsRepo(ws) {
		return "", nil
	}
	if !Dirty(ws) {
		return "", nil
	}
	if !hasIdentity(ws) {
		return "", fmt.Errorf("git user.name/user.email are not set — yagent refuses to guess a git identity (git config user.name \"...\" && git config user.email \"...\")")
	}
	// stage all, commit; -A picks up untracked, --no-verify skips pre-commit hooks.
	if _, err := run(ws, "add", "-A"); err != nil {
		return "", err
	}
	if subject == "" {
		subject = "work in progress"
	}
	args := []string{"commit", "--no-verify", "-m", subject}
	if _, err := run(ws, args...); err != nil {
		return "", err
	}
	out, err := run(ws, "rev-parse", "--short", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// hasIdentity checks user.name and user.email are configured (local or global).
func hasIdentity(ws string) bool {
	for _, k := range []string{"user.name", "user.email"} {
		if _, err := run(ws, "config", "--get", k); err != nil {
			return false
		}
	}
	return true
}

// AgentCommits lists the most recent agent-authored commits (newest first),
// matching the Marker prefix on the message subject.
func AgentCommits(ws string) ([]Commit, error) {
	if !IsRepo(ws) {
		return nil, nil
	}
	out, err := run(ws, "log", "--format=%h %s", "-n", "50")
	if err != nil {
		return nil, err
	}
	var commits []Commit
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimSpace(ln)
		// "%h %s" -> "<7-char hash> <subject>"
		if len(ln) < 9 {
			continue
		}
		hash, subject := ln[:7], ln[8:]
		if !strings.HasPrefix(subject, Marker+": ") {
			continue
		}
		commits = append(commits, Commit{Hash: hash, Subject: subject})
	}
	return commits, nil
}

// RevertN reverts the n most recent agent-authored commits (all of them when
// n <= 0). It finds agent commits by marker and uses `git revert --no-commit`
// on the contiguous run, so each reverts cleanly even when the user committed
// in between. Returns the reverted subjects.
func RevertN(ws string, n int) ([]string, error) {
	commits, err := AgentCommits(ws)
	if err != nil {
		return nil, err
	}
	if len(commits) == 0 {
		return nil, fmt.Errorf("nothing to undo")
	}
	if n <= 0 || n > len(commits) {
		n = len(commits)
	}
	target := commits[:n] // newest first
	// Revert in oldest→newest order of the run (reverse of the slice).
	var reverted []string
	for i := len(target) - 1; i >= 0; i-- {
		c := target[i]
		if _, err := run(ws, "revert", "--no-commit", c.Hash); err != nil {
			// A revert can conflict; drop the leftover revert state so the
			// user is left with a working tree, not a mid-revert lock.
			_, _ = run(ws, "revert", "--abort")
			return reverted, fmt.Errorf("revert %s failed (conflict): %w", c.Hash, err)
		}
		reverted = append(reverted, c.Subject)
	}
	// commit the accumulated revert in one agent-marked commit so it is itself
	// reversible.
	_, _ = commit(ws, Marker+": undo", false)
	return reverted, nil
}

// DiffSince shows the diff of the given ref (a hash or HEAD~N) against HEAD,
// for the /diff-style review view.
func DiffSince(ws, ref string) string {
	out, err := run(ws, "diff", ref+"...HEAD")
	if err != nil {
		return ""
	}
	return out
}

func run(ws string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = ws
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(errBuf.String()), err
	}
	return out.String(), nil
}

// InitIfNeeded initializes a git repo in ws when none exists (aider does this
// so undo always works). Returns true if it created one.
func InitIfNeeded(ws string) bool {
	if IsRepo(ws) {
		return false
	}
	if _, err := run(ws, "init"); err != nil {
		return false
	}
	return true
}

// Root returns the git work-tree root for ws, or "" when not a repo.
func Root(ws string) string {
	out, err := run(ws, "rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}
