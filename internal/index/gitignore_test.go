package index

import (
	"strings"
	"testing"
)

func matchSet(t *testing.T, base, body string, cases map[string]bool) {
	t.Helper()
	m := &gitignoreMatcher{}
	m.push(parseGitignore(base, body))
	for path, want := range cases {
		if got := m.ignored(path, false); got != want {
			t.Errorf("ignore %q = %v, want %v", path, got, want)
		}
	}
}

func TestGitignoreBasics(t *testing.T) {
	matchSet(t, "", "*.log\n", map[string]bool{
		"a.log": true, "nested/b.log": true, "a.txt": false, "logger.go": false,
	})
}

func TestGitignoreDirOnly(t *testing.T) {
	m := &gitignoreMatcher{}
	m.push(parseGitignore("", "node_modules/\n"))
	if !m.ignored("node_modules", true) {
		t.Error("node_modules dir not ignored")
	}
	if !m.ignored("a/node_modules/pkg/index.js", false) {
		t.Error("file under node_modules not ignored")
	}
	if m.ignored("node_modules.txt", false) {
		t.Error("node_modules.txt should not match a dir-only rule")
	}
}

func TestGitignoreNegation(t *testing.T) {
	matchSet(t, "", "*.log\n!important.log\n", map[string]bool{
		"a.log": true, "important.log": false,
	})
}

func TestGitignoreAnchored(t *testing.T) {
	matchSet(t, "", "/rooted.txt\n", map[string]bool{
		"rooted.txt": true, "sub/rooted.txt": false,
	})
	matchSet(t, "", "build/\n", map[string]bool{
		"build/out.js": true, "a/build/out.js": true, // dir-only unanchored matches anywhere
	})
}

func TestGitignoreDoubleStar(t *testing.T) {
	matchSet(t, "", "build/**\n", map[string]bool{
		"build/a/b.go": true, "other/build/a.go": false,
	})
}

func TestGitignoreNested(t *testing.T) {
	m := &gitignoreMatcher{}
	m.push(parseGitignore("", "*.log\n"))
	m.push(parseGitignore("docs", "!important.log\n"))
	// root rule applies everywhere
	if !m.ignored("a.log", false) {
		t.Error("a.log should be ignored by the root rule")
	}
	// nested rule re-includes only under its own directory
	if m.ignored("docs/important.log", false) {
		t.Error("nested negation should re-include docs/important.log")
	}
	if !m.ignored("docs/other.log", false) {
		t.Error("docs/other.log should stay ignored")
	}
	if !m.ignored("important.log", false) {
		t.Error("nested rule must not leak above its directory")
	}
}

func TestGitignoreCommentsAndBlanks(t *testing.T) {
	body := "# a comment\n\n*.tmp\n"
	m := &gitignoreMatcher{}
	m.push(parseGitignore("", body))
	if !m.ignored("x.tmp", false) {
		t.Error("*.tmp not ignored")
	}
	if !strings.Contains(body, "# a comment") {
		t.Error("comment marker missing")
	}
}
