package skills

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// slugRe is the skill-name rule (same as Hermes/agentskills slugs).
var slugRe = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

var semverRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// Size caps from docs/design/skills.md.
const (
	MaxSkillBytes = 8 << 10
	MaxRefBytes   = 16 << 10
)

// frontmatter is the agentskills.io-compatible subset of SKILL.md metadata.
// source/created_at/last_used/failures are store-managed: the model never sets
// them.
type frontmatter struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Version     string   `yaml:"version,omitempty"`
	Tags        []string `yaml:"tags,omitempty"`
	Category    string   `yaml:"category,omitempty"`
	Source      string   `yaml:"source,omitempty"`
	CreatedAt   int64    `yaml:"created_at,omitempty"`
	LastUsed    int64    `yaml:"last_used,omitempty"`
	Failures    int      `yaml:"failures,omitempty"`
	Pinned      bool     `yaml:"pinned,omitempty"`
}

// parseFrontmatter splits content into the metadata block and the body.
func parseFrontmatter(content string) (frontmatter, string, error) {
	var fm frontmatter
	rest := strings.TrimPrefix(content, "\ufeff")
	if !strings.HasPrefix(rest, "---") {
		return fm, "", vf("missing frontmatter (SKILL.md must start with a '---' block)")
	}
	rest = strings.TrimPrefix(rest, "---")
	rest = strings.TrimPrefix(rest, "\n")
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return fm, "", vf("unterminated frontmatter (missing closing '---')")
	}
	block := rest[:end]
	body := strings.TrimPrefix(rest[end+4:], "\n")
	if err := yaml.Unmarshal([]byte(block), &fm); err != nil {
		return fm, "", fmt.Errorf("parse frontmatter: %w", err)
	}
	return fm, body, nil
}

// renderSkill serializes a skill with a stable frontmatter order and a
// trailing newline on the body.
func renderSkill(fm frontmatter, body string) string {
	var b strings.Builder
	b.WriteString("---\n")
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	_ = enc.Encode(fm)
	enc.Close()
	b.WriteString("---\n")
	b.WriteString(strings.Trim(body, "\n"))
	b.WriteString("\n")
	return b.String()
}

// validateFM checks the model-facing metadata fields.
func validateFM(fm frontmatter) error {
	if !slugRe.MatchString(fm.Name) {
		return vf("invalid name %q: must match [a-z][a-z0-9_-]*", fm.Name)
	}
	if fm.Description == "" {
		return vf("description is required (one line, at most 60 characters, stating the trigger condition)")
	}
	if r := []rune(fm.Description); len(r) > 60 {
		return vf("description is %d characters; max 60", len(r))
	}
	if strings.ContainsAny(fm.Description, "\r\n") {
		return vf("description must be a single line")
	}
	if fm.Version != "" && !semverRe.MatchString(fm.Version) {
		return vf("version %q is not semver (e.g. 1.0.0)", fm.Version)
	}
	if fm.Category != "" && !slugRe.MatchString(fm.Category) {
		return vf("category %q is invalid: must match [a-z][a-z0-9_-]*", fm.Category)
	}
	return nil
}

// validateBody requires the two sections the model fills in.
func validateBody(body string) error {
	if !strings.Contains(body, "## When to Use") {
		return vf("SKILL.md body must contain a '## When to Use' section")
	}
	if !strings.Contains(body, "## Procedure") {
		return vf("SKILL.md body must contain a '## Procedure' section")
	}
	return nil
}
