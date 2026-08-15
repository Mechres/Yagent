package tools

import (
	"strings"
	"testing"
)

func TestPreflightImportsGo(t *testing.T) {
	// fmt used without import -> flagged
	missing := preflightImports("main.go", "package main\nfunc main() { fmt.Println(\"hi\") }\n")
	if !strings.Contains(missing, `"fmt"`) {
		t.Errorf("missing fmt not flagged: %q", missing)
	}
	// import present -> no flag
	if msg := preflightImports("main.go", "package main\nimport \"fmt\"\nfunc main() { fmt.Println(\"hi\") }\n"); msg != "" {
		t.Errorf("imported fmt wrongly flagged: %q", msg)
	}
	// fmt inside a string literal must NOT be flagged
	if msg := preflightImports("main.go", "package main\nfunc main() { _ = \"fmt.Println is text\" }\n"); msg != "" {
		t.Errorf("string-literal fmt wrongly flagged: %q", msg)
	}
	// fmt inside a comment must NOT be flagged
	if msg := preflightImports("main.go", "package main\n// fmt.Println not a call\nfunc main() {}\n"); msg != "" {
		t.Errorf("comment fmt wrongly flagged: %q", msg)
	}
	// non-stdlib qualifier (workspace pkg) must not be flagged
	if msg := preflightImports("main.go", "package main\nfunc main() { mypkg.Run() }\n"); msg != "" {
		t.Errorf("workspace package wrongly flagged: %q", msg)
	}
}

func TestPreflightImportsPython(t *testing.T) {
	// os used without import -> flagged
	missing := preflightImports("a.py", "import sys\n\ndef f():\n    return os.getenv(\"HOME\")\n")
	if !strings.Contains(missing, "os") {
		t.Errorf("missing os not flagged: %q", missing)
	}
	// imported -> no flag
	if msg := preflightImports("a.py", "import os\n\ndef f():\n    return os.getenv(\"HOME\")\n"); msg != "" {
		t.Errorf("imported os wrongly flagged: %q", msg)
	}
	// from-import does NOT bind the module name -> os.getenv is still an error
	if msg := preflightImports("a.py", "from os import getenv\n\ndef f():\n    return os.getenv(\"HOME\")\n"); msg == "" {
		t.Error("os referenced after only `from os import` should be flagged")
	}
	// `import os` alongside a from-import clears it
	if msg := preflightImports("a.py", "from os import getenv\nimport os\n\ndef f():\n    return os.getenv(\"HOME\")\n"); msg != "" {
		t.Errorf("os imported (bare import) wrongly flagged: %q", msg)
	}
	// os inside a string must not be flagged
	if msg := preflightImports("a.py", "x = \"os.getenv is just text\"\n"); msg != "" {
		t.Errorf("string os wrongly flagged: %q", msg)
	}
	// non-stdlib qualifier not flagged
	if msg := preflightImports("a.py", "import requests\nr = requests.get(\"x\")\n"); msg != "" {
		t.Errorf("third-party package wrongly flagged: %q", msg)
	}
}

func TestPreflightImportsNonGoPy(t *testing.T) {
	if msg := preflightImports("a.js", "import x from 'y'"); msg != "" {
		t.Errorf("js wrongly checked: %q", msg)
	}
}

func TestPreflightPlaceholders(t *testing.T) {
	// AGY P0: a model abbreviating a large file with "// ... existing code ..."
	// must be blocked — it would silently truncate the file, defeating every
	// downstream gate. Covered variants: comment-started ellipsis (all comment
	// tokens), and bare prose mixing an ellipsis with truncation keywords.
	blocked := []string{
		"package main\n\n// ... existing code ...\n",
		"package main\n\n# ... rest of the file unchanged\n",
		"func main() {\n\t// ...\n}\n",
		"...rest of the code remains the same...\n",
		"keep the rest of the file...\n",
		"/* ... unchanged code below */\n",
		"// unchanged code above ...\n",
	}
	for _, content := range blocked {
		if msg := preflightPlaceholders("a.go", content); msg == "" {
			t.Errorf("placeholder NOT blocked: %q", content)
		}
	}
	// False-positive safety: real code with literal ellipses must pass.
	clean := []string{
		"package main\n\nfunc main() {\n\tf(a...)\n\t_ = 3.14\n}\n",
		"def stub(self, *args, **kwargs):\n    ...\n", // Python Ellipsis, no keyword
		"// Example: a, b, c... the sequence\n",
		"x := \"...just text inside a string literal\"\n",
		"x := []int{1, 2, 3}\n",
	}
	for _, content := range clean {
		if msg := preflightPlaceholders("a.py", content); msg != "" {
			t.Errorf("clean content wrongly blocked: %q -> %q", content, msg)
		}
	}
}
