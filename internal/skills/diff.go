package skills

import (
	"fmt"
	"strings"
)

const diffContext = 3

// lineDiff renders a unified diff between old and new text, labeled with the
// skill path, for /skills diff review. Uses a full LCS table; fine for the
// sub-8 KiB files the store caps.
func lineDiff(label, oldText, newText string) string {
	a := splitLines(oldText)
	b := splitLines(newText)
	ops := lcsOps(a, b)
	if len(ops) == 0 {
		return fmt.Sprintf("--- %s\n+++ %s\n(no changes)\n", label, label)
	}

	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", label, label)
	for _, h := range hunks(ops) {
		aStart, aCount := hunkLines(h, '-')
		bStart, bCount := hunkLines(h, '+')
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", aStart, aCount, bStart, bCount)
		for _, op := range h {
			out.WriteByte(op.kind)
			out.WriteString(op.line)
			out.WriteByte('\n')
		}
	}
	return out.String()
}

type diffOp struct {
	kind byte // ' ', '-', '+'
	line string
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func lcsOps(a, b []string) []diffOp {
	n, m := len(a), len(b)
	// Guard: for very large inputs fall back to a naive comparison.
	if n*m > 4_000_000 {
		return naiveOps(a, b)
	}
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	var ops []diffOp
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{' ', a[i]})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			ops = append(ops, diffOp{'-', a[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', b[j]})
			j++
		}
	}
	for i < n {
		ops = append(ops, diffOp{'-', a[i]})
		i++
	}
	for j < m {
		ops = append(ops, diffOp{'+', b[j]})
		j++
	}
	return ops
}

// naiveOps compares line-by-line; used only when the LCS table would be huge.
func naiveOps(a, b []string) []diffOp {
	var ops []diffOp
	max := len(a)
	if len(b) > max {
		max = len(b)
	}
	for i := 0; i < max; i++ {
		var al, bl string
		hasA, hasB := i < len(a), i < len(b)
		if hasA {
			al = a[i]
		}
		if hasB {
			bl = b[i]
		}
		switch {
		case hasA && hasB && al == bl:
			ops = append(ops, diffOp{' ', al})
		case hasA:
			ops = append(ops, diffOp{'-', al})
			if hasB {
				ops = append(ops, diffOp{'+', bl})
			}
		case hasB:
			ops = append(ops, diffOp{'+', bl})
		}
	}
	return ops
}

// hunks groups a diff op sequence into change regions with up to diffContext
// context lines around them.
func hunks(ops []diffOp) [][]diffOp {
	var out [][]diffOp
	i := 0
	for i < len(ops) {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		start := i
		for back := 1; back <= diffContext && i-back >= 0 && ops[i-back].kind == ' '; back++ {
			start = i - back
		}
		j := i
		for j < len(ops) {
			for j < len(ops) && ops[j].kind != ' ' {
				j++
			}
			k := 0
			for k < diffContext && j+k < len(ops) && ops[j+k].kind == ' ' {
				k++
			}
			j += k
			if j >= len(ops) || ops[j].kind != ' ' {
				break
			}
		}
		out = append(out, ops[start:j])
		i = j
	}
	return out
}

// hunkLines computes the @@ header numbers for one hunk.
func hunkLines(h []diffOp, kind byte) (start, count int) {
	aLine, bLine := 1, 1
	countA, countB := 0, 0
	firstA, firstB := 0, 0
	for _, op := range h {
		switch op.kind {
		case ' ':
			if firstA == 0 {
				firstA, firstB = aLine, bLine
			}
			aLine++
			bLine++
		case '-':
			if firstA == 0 {
				firstA, firstB = aLine, bLine
			}
			countA++
			aLine++
		case '+':
			if firstB == 0 {
				firstB = bLine
			}
			countB++
			bLine++
		}
	}
	if firstA == 0 {
		firstA = 1
	}
	if firstB == 0 {
		firstB = 1
	}
	if kind == '-' {
		return firstA, countA + contextCount(h)
	}
	return firstB, countB + contextCount(h)
}

func contextCount(h []diffOp) int {
	n := 0
	for _, op := range h {
		if op.kind == ' ' {
			n++
		}
	}
	return n
}
