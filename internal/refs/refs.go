// Package refs resolves bounded, deterministic @ context references.
package refs

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	maxReferenceBytes = 8 << 10
	maxTotalBytes     = 12 << 10
	maxFolderEntries  = 80
)

var tokenRE = regexp.MustCompile(`@(?:file|folder):[^\s]+|@(?:diff|staged)\b`)
var rangeRE = regexp.MustCompile(`^(.*):(\d+)-(\d+)$`)

// Resolve expands references found in input into a bounded system-context
// block. Unknown or unsafe references become explicit errors in the block;
// they never read outside workspace or execute model-supplied commands.
func Resolve(workspace, input string) string {
	tokens := tokenRE.FindAllString(input, -1)
	if len(tokens) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("[USER REFERENCES — deterministic context]\n")
	used := 0
	for _, token := range tokens {
		if used >= maxTotalBytes {
			b.WriteString("… additional references omitted (context cap)\n")
			break
		}
		content, label := resolveOne(workspace, token)
		remaining := maxTotalBytes - used
		if len(content) > remaining {
			content = content[:remaining] + "\n… reference truncated (context cap)"
		}
		fmt.Fprintf(&b, "\n--- %s ---\n%s\n", label, content)
		used += len(content)
	}
	return b.String()
}

func resolveOne(workspace, token string) (string, string) {
	if token == "@diff" || token == "@staged" {
		args := []string{"diff"}
		if token == "@staged" {
			args = append(args, "--cached")
		}
		cmd := exec.Command("git", args...)
		cmd.Dir = workspace
		out, err := cmd.Output()
		if err != nil {
			return "error: git diff unavailable", token
		}
		return capText(string(out)), token
	}
	raw := strings.TrimPrefix(token, "@file:")
	kind := "file"
	if strings.HasPrefix(token, "@folder:") {
		kind, raw = "folder", strings.TrimPrefix(token, "@folder:")
	}
	lineStart, lineEnd := 0, 0
	if kind == "file" {
		if m := rangeRE.FindStringSubmatch(raw); m != nil {
			raw = m[1]
			lineStart, _ = strconv.Atoi(m[2])
			lineEnd, _ = strconv.Atoi(m[3])
			if lineStart < 1 || lineEnd < lineStart {
				return "error: invalid line range", token
			}
		}
	}
	path, err := confinedPath(workspace, raw)
	if err != nil {
		return "error: " + err.Error(), token
	}
	if kind == "folder" {
		return folderListing(path), token
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "error: " + err.Error(), token
	}
	if strings.IndexByte(string(data), 0) >= 0 {
		return "error: binary files are not supported", token
	}
	text := string(data)
	if lineStart > 0 {
		lines := strings.Split(text, "\n")
		if lineStart > len(lines) {
			return "error: line range starts past end of file", token
		}
		if lineEnd > len(lines) {
			lineEnd = len(lines)
		}
		text = strings.Join(lines[lineStart-1:lineEnd], "\n")
	}
	return capText(text), token
}

func confinedPath(workspace, name string) (string, error) {
	name = strings.Trim(strings.TrimSpace(name), "\"'")
	if name == "" || filepath.IsAbs(name) {
		return "", fmt.Errorf("path must be workspace-relative")
	}
	clean := filepath.Clean(name)
	if sensitive(clean) {
		return "", fmt.Errorf("sensitive path blocked")
	}
	root, _ := filepath.Abs(workspace)
	path, _ := filepath.Abs(filepath.Join(root, clean))
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes workspace")
	}
	return path, nil
}

func sensitive(path string) bool {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		lower := strings.ToLower(part)
		if lower == ".env" || lower == ".ssh" || lower == ".aws" || lower == ".gnupg" || lower == ".git-credentials" || strings.HasPrefix(lower, "id_rsa") {
			return true
		}
	}
	return false
}

func folderListing(path string) string {
	var entries []string
	_ = filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil || len(entries) >= maxFolderEntries {
			return filepath.SkipDir
		}
		if p == path {
			return nil
		}
		rel, _ := filepath.Rel(path, p)
		if d.IsDir() && (d.Name() == ".git" || d.Name() == ".yagent") {
			return filepath.SkipDir
		}
		if !sensitive(rel) {
			entries = append(entries, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(entries)
	return strings.Join(entries, "\n")
}

func capText(s string) string {
	if len(s) <= maxReferenceBytes {
		return s
	}
	return s[:maxReferenceBytes] + "\n… reference truncated"
}
