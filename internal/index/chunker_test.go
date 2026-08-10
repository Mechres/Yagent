package index

import (
	"strings"
	"testing"
)

const goFixture = `package demo

import "fmt"

// helper returns x+1.
func helper(x int) int { return x + 1 }

// config holds the workspace settings.
type config struct{ ws string }

func (c *config) run() string { return c.ws }

var version = "1.0"

func main() {
	fmt.Println(helper(1))
}
`

func TestChunkerStructural(t *testing.T) {
	chunks := chunkSource("demo.go", goFixture)
	if len(chunks) < 3 {
		t.Fatalf("chunks = %d, want >= 3 (helper, config, merged small decls)", len(chunks))
	}
	found := false
	for _, c := range chunks {
		if strings.Contains(c.Content, "// helper returns x+1.") && strings.Contains(c.Content, "func helper") {
			found = true
			if c.StartLine != 5 {
				t.Errorf("helper chunk starts at line %d, want 5 (doc comment)", c.StartLine)
			}
			break
		}
	}
	if !found {
		t.Errorf("no chunk contains the helper doc comment + declaration: %+v", chunks)
	}
	for _, c := range chunks {
		if c.StartLine < 1 || c.EndLine < c.StartLine {
			n := len(c.Content)
			if n > 20 {
				n = 20
			}
			t.Errorf("bad line range %d-%d for chunk %q", c.StartLine, c.EndLine, c.Content[:n])
		}
	}
}

func TestChunkerFallbackLineWindows(t *testing.T) {
	var lines []string
	for i := 1; i <= 200; i++ {
		lines = append(lines, strings.Repeat("line ", 5)+strings.Repeat("x", 10))
	}
	chunks := chunkSource("notes.md", strings.Join(lines, "\n"))
	// windows are bounded by both 80 lines and ~1200 chars
	if chunks[0].StartLine != 1 || chunks[len(chunks)-1].EndLine != 200 {
		t.Fatalf("windows do not cover the file: first %d last %d, want 1..200", chunks[0].StartLine, chunks[len(chunks)-1].EndLine)
	}
	for _, c := range chunks {
		if len(c.Content) > maxChunkChars {
			t.Errorf("chunk %d chars exceeds cap %d", len(c.Content), maxChunkChars)
		}
		if c.EndLine-c.StartLine+1 > fallbackWindow {
			t.Errorf("chunk spans %d lines, window is %d", c.EndLine-c.StartLine+1, fallbackWindow)
		}
	}
}

func TestChunkerSplitsOversizedDeclaration(t *testing.T) {
	var b strings.Builder
	b.WriteString("// long function with a very long body\nfunc huge(x int) int {\n")
	for i := 0; i < 300; i++ {
		b.WriteString("    _ = x + " + strings.Repeat("1", 30) + "\n")
	}
	b.WriteString("    return x\n}\n")
	chunks := chunkSource("huge.go", b.String())
	if len(chunks) < 2 {
		t.Fatalf("oversized decl not split, got %d chunks", len(chunks))
	}
	for _, c := range chunks {
		if len(c.Content) > maxChunkChars {
			t.Errorf("chunk %d chars exceeds cap %d", len(c.Content), maxChunkChars)
		}
	}
}
