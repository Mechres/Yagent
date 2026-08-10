package index

import (
	"regexp"
	"strings"
)

// gitignoreRule is one parsed pattern from a .gitignore file. Matching follows
// the common subset: comments, blank lines, negation (!), trailing-slash
// dir-only rules, and glob (*, **, ?). Anchored patterns (containing a '/')
// match relative to the .gitignore's directory; unanchored ones match at any
// depth below it. The last matching rule wins.
type gitignoreRule struct {
	negate  bool
	dirOnly bool
	// re matches the path itself; reChild (dirOnly only) matches anything
	// underneath the named directory.
	re      *regexp.Regexp
	reChild *regexp.Regexp
}

// gitignoreMatcher accumulates rules from nested .gitignore files as the walk
// descends, so child directories layer on top of parent rules.
type gitignoreMatcher struct {
	stack []gitignoreRuleset
}

type gitignoreRuleset struct {
	base  string // dir the .gitignore lives in, slash-separated
	rules []gitignoreRule
}

// parseGitignore parses a .gitignore file body rooted at base ("" = workspace
// root). Invalid patterns are skipped.
func parseGitignore(base, body string) gitignoreRuleset {
	var rs gitignoreRuleset
	rs.base = strings.Trim(base, "/")
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rule, ok := compileGitignoreRule(line)
		if ok {
			rs.rules = append(rs.rules, rule)
		}
	}
	return rs
}

func compileGitignoreRule(line string) (gitignoreRule, bool) {
	r := gitignoreRule{}
	if strings.HasPrefix(line, "!") {
		r.negate = true
		line = line[1:]
	}
	if line == "" {
		return r, false
	}
	// A trailing / marks a dir-only rule (matches the dir and everything under it).
	if strings.HasSuffix(line, "/") {
		r.dirOnly = true
		line = strings.TrimSuffix(line, "/")
	}
	// A pattern containing a '/' (including a leading one) is anchored to the
	// .gitignore's directory; otherwise it matches at any depth below it.
	anchored := strings.Contains(line, "/")
	line = strings.TrimPrefix(line, "/")
	pattern := globToRegex(line)
	build := func(prefix, suffix string) *regexp.Regexp {
		return regexp.MustCompile(prefix + pattern + suffix)
	}
	switch {
	case r.dirOnly:
		// The dir itself (any depth when unanchored) plus everything under it.
		if anchored {
			r.re = build(`^`, `$`)
			r.reChild = build(`^`, `/.*$`)
		} else {
			r.re = build(`^(?:.*/)?`, `$`)
			r.reChild = build(`^(?:.*/)?`, `/.*$`)
		}
	case anchored:
		r.re = build(`^`, `$`)
	default:
		r.re = build(`^(?:.*/)?`, `$`)
	}
	return r, true
}

// globToRegex converts the gitignore glob subset to a regex body.
func globToRegex(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch c {
		case '*':
			if i+1 < len(p) && p[i+1] == '*' {
				b.WriteString(`.*`)
				i++
			} else {
				b.WriteString(`[^/]*`)
			}
		case '?':
			b.WriteString(`[^/]`)
		case '[', ']', '.', '+', '(', ')', '{', '}', '^', '$', '|', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// ignored reports whether rel (slash-separated, relative to the workspace
// root) is ignored. isDir tells dir-only rules whether to apply.
func (m *gitignoreMatcher) ignored(rel string, isDir bool) bool {
	ignored := false
	for _, rs := range m.stack {
		if rs.base != "" && !strings.HasPrefix(rel, rs.base+"/") {
			continue
		}
		local := rel
		if rs.base != "" {
			local = strings.TrimPrefix(local, rs.base+"/")
		}
		for _, rule := range rs.rules {
			if rule.matches(local, isDir) {
				ignored = !rule.negate
			}
		}
	}
	return ignored
}

func (r *gitignoreRule) matches(rel string, isDir bool) bool {
	if r.reChild != nil {
		// A dir-only rule ignores the directory itself and everything under it.
		if r.reChild.MatchString(rel) {
			return true
		}
		return isDir && r.re.MatchString(rel)
	}
	return r.re.MatchString(rel)
}

// push adds a ruleset for a directory being entered.
func (m *gitignoreMatcher) push(rs gitignoreRuleset) {
	m.stack = append(m.stack, rs)
}

// pop removes the most recently pushed ruleset.
func (m *gitignoreMatcher) pop() {
	if len(m.stack) > 0 {
		m.stack = m.stack[:len(m.stack)-1]
	}
}

// empty reports whether no rules are in effect yet.
func (m *gitignoreMatcher) empty() bool {
	return len(m.stack) == 0
}
