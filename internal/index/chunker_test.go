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

func TestChunkerRustAndJava(t *testing.T) {
	rust := `fn main() {
    let greeting = "hello from the entry point";
    println!("{}", greeting);
    println!("this body is long enough to be its own chunk");
}

struct Point {
    x: f64,
    y: f64,
    label: String,
}

impl Point {
    fn new(x: f64, y: f64) -> Self {
        Point { x, y, label: String::from("origin") }
    }
}
`
	chunks := chunkSource("main.rs", rust)
	if len(chunks) < 3 {
		t.Fatalf("rust chunks = %d, want >= 3 (fn, struct, impl)", len(chunks))
	}
	found := false
	for _, c := range chunks {
		if strings.Contains(c.Content, "fn main()") {
			found = true
			break
		}
	}
	if !found {
		t.Error("rust fn main not chunked structurally")
	}

	java := `package demo;

public class App {
    public static void main(String[] args) {
        System.out.println("hi");
    }

    private int add(int a, int b) {
        return a + b;
    }
}
`
	jchunks := chunkSource("App.java", java)
	// Java methods nest inside the class, so a single class is one structural
	// chunk; assert the parser ran and kept the whole class.
	if len(jchunks) != 1 || !strings.Contains(jchunks[0].Content, "public static void main") ||
		!strings.Contains(jchunks[0].Content, "private int add") {
		t.Fatalf("java chunks = %+v", jchunks)
	}
}

func TestChunkerCandCpp(t *testing.T) {
	c := `#include <stdio.h>

int main(void) {
    printf("hello from main, this function body is long enough\n");
    printf("a second line to make the chunk exceed the tiny-file threshold\n");
    return 0;
}

typedef struct Point {
    double x;
    double y;
} Point;
`
	if chunks := chunkSource("main.c", c); len(chunks) < 2 {
		t.Fatalf("c chunks = %d, want >= 2", len(chunks))
	}
	cpp := `#include <vector>
#include <string>

namespace demo {
class Widget {
public:
    Widget(const std::string& name);
    int value() const;
private:
    std::string name_;
    int value_ = 0;
};
}

int free_helper() {
    return 42;
}
`
	if chunks := chunkSource("widget.cpp", cpp); len(chunks) < 2 {
		t.Fatalf("cpp chunks = %d, want >= 2", len(chunks))
	}
}
