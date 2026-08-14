package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

func splitKeepEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimRight(s, "\n"), "\n")
}

type diffOp int

const (
	diffEqual diffOp = iota
	diffInsert
	diffDelete
)

type diffEdit struct {
	op   diffOp
	text string
}

// computeLCS calculates the line-by-line diff between a and b using Longest
// Common Subsequence with common prefix/suffix trimming.
func computeLCS(a, b []string) []diffEdit {
	n, m := len(a), len(b)
	if n == 0 && m == 0 {
		return nil
	}
	if n == 0 {
		edits := make([]diffEdit, m)
		for i, line := range b {
			edits[i] = diffEdit{op: diffInsert, text: line}
		}
		return edits
	}
	if m == 0 {
		edits := make([]diffEdit, n)
		for i, line := range a {
			edits[i] = diffEdit{op: diffDelete, text: line}
		}
		return edits
	}

	// Trim common prefix
	prefixLen := 0
	for prefixLen < n && prefixLen < m && a[prefixLen] == b[prefixLen] {
		prefixLen++
	}
	// Trim common suffix
	suffixLen := 0
	for suffixLen < (n-prefixLen) && suffixLen < (m-prefixLen) && a[n-1-suffixLen] == b[m-1-suffixLen] {
		suffixLen++
	}

	midA := a[prefixLen : n-suffixLen]
	midB := b[prefixLen : m-suffixLen]

	var midEdits []diffEdit
	if len(midA) > 0 && len(midB) > 0 {
		dp := make([][]int, len(midA)+1)
		for i := range dp {
			dp[i] = make([]int, len(midB)+1)
		}
		for i := 1; i <= len(midA); i++ {
			for j := 1; j <= len(midB); j++ {
				if midA[i-1] == midB[j-1] {
					dp[i][j] = dp[i-1][j-1] + 1
				} else if dp[i-1][j] >= dp[i][j-1] {
					dp[i][j] = dp[i-1][j]
				} else {
					dp[i][j] = dp[i][j-1]
				}
			}
		}

		i, j := len(midA), len(midB)
		for i > 0 || j > 0 {
			if i > 0 && j > 0 && midA[i-1] == midB[j-1] {
				midEdits = append(midEdits, diffEdit{op: diffEqual, text: midA[i-1]})
				i--
				j--
			} else if j > 0 && (i == 0 || dp[i][j-1] >= dp[i-1][j]) {
				midEdits = append(midEdits, diffEdit{op: diffInsert, text: midB[j-1]})
				j--
			} else if i > 0 && (j == 0 || dp[i-1][j] >= dp[i][j-1]) {
				midEdits = append(midEdits, diffEdit{op: diffDelete, text: midA[i-1]})
				i--
			}
		}
		// reverse midEdits
		for l, r := 0, len(midEdits)-1; l < r; l, r = l+1, r-1 {
			midEdits[l], midEdits[r] = midEdits[r], midEdits[l]
		}
	} else if len(midA) > 0 {
		for _, line := range midA {
			midEdits = append(midEdits, diffEdit{op: diffDelete, text: line})
		}
	} else if len(midB) > 0 {
		for _, line := range midB {
			midEdits = append(midEdits, diffEdit{op: diffInsert, text: line})
		}
	}

	edits := make([]diffEdit, 0, n+m)
	for k := 0; k < prefixLen; k++ {
		edits = append(edits, diffEdit{op: diffEqual, text: a[k]})
	}
	edits = append(edits, midEdits...)
	for k := n - suffixLen; k < n; k++ {
		edits = append(edits, diffEdit{op: diffEqual, text: a[k]})
	}
	return edits
}

// renderApprovalDiff is a colorized hunk diff (additions in theme green,
// removals in theme red) with context lines for approval previews.
func renderApprovalDiff(th Theme, oldText, newText string) string {
	oldLines := splitKeepEmpty(oldText)
	newLines := splitKeepEmpty(newText)
	if len(oldLines) == 0 && len(newLines) == 0 {
		return "(no changes)"
	}
	edits := computeLCS(oldLines, newLines)
	if len(edits) == 0 {
		return "(no changes)"
	}

	hasChanges := false
	for _, e := range edits {
		if e.op != diffEqual {
			hasChanges = true
			break
		}
	}
	if !hasChanges {
		return "(no changes)"
	}

	green := lipgloss.NewStyle().Foreground(th.Success)
	red := lipgloss.NewStyle().Foreground(th.Error)
	hunk := lipgloss.NewStyle().Foreground(th.Secondary)

	const contextLines = 3
	var b strings.Builder
	b.WriteString(hunk.Render("── diff ──") + "\n")

	type hunkRange struct {
		start, end int
	}
	var changeIndices []int
	for idx, e := range edits {
		if e.op != diffEqual {
			changeIndices = append(changeIndices, idx)
		}
	}

	var ranges []hunkRange
	for _, ci := range changeIndices {
		start := ci - contextLines
		if start < 0 {
			start = 0
		}
		end := ci + contextLines + 1
		if end > len(edits) {
			end = len(edits)
		}
		if len(ranges) > 0 && start <= ranges[len(ranges)-1].end {
			ranges[len(ranges)-1].end = end
		} else {
			ranges = append(ranges, hunkRange{start: start, end: end})
		}
	}

	for rIdx, r := range ranges {
		if rIdx > 0 {
			b.WriteString(hunk.Render("···") + "\n")
		}
		for i := r.start; i < r.end; i++ {
			e := edits[i]
			switch e.op {
			case diffEqual:
				b.WriteString("  " + e.text + "\n")
			case diffDelete:
				b.WriteString(red.Render("- "+e.text) + "\n")
			case diffInsert:
				b.WriteString(green.Render("+ "+e.text) + "\n")
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
