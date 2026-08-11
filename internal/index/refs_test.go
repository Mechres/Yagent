package index

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCallRefsGo(t *testing.T) {
	src := `package pkg

func main() {
	greet("world")
	fmt.Println("hi")
	obj.Fetch(1)
	_ = greet
}
`
	lang := languageFor("a.go")
	_, root, tree := parseDeclsWithTree("a.go", src, lang)
	defer tree.Close()
	refs := refsFromTree("a.go", lang, root, []byte(src))
	callees := map[string]bool{}
	for _, r := range refs {
		callees[r.Callee] = true
	}
	for _, want := range []string{"greet", "Println", "Fetch"} {
		if !callees[want] {
			t.Errorf("missing call to %q; got %v", want, refs)
		}
	}
}

func TestCallRefsPython(t *testing.T) {
	src := `def main():
    helper(1)
    obj.method(2)
    return
`
	lang := languageFor("a.py")
	_, root, tree := parseDeclsWithTree("a.py", src, lang)
	defer tree.Close()
	refs := refsFromTree("a.py", lang, root, []byte(src))
	callees := map[string]bool{}
	for _, r := range refs {
		callees[r.Callee] = true
	}
	for _, want := range []string{"helper", "method"} {
		if !callees[want] {
			t.Errorf("missing python call %q; got %v", want, refs)
		}
	}
}

func TestReferencesQuery(t *testing.T) {
	ts, _ := countingEmbedServer(t)
	ws := t.TempDir()
	store, err := Open(ws, t.TempDir(), ts.URL, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	src := "package pkg\n\nfunc A() { B() }\nfunc B() {}\n"
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Index(context.Background()); err != nil {
		t.Fatal(err)
	}
	refs := store.References(context.Background(), "B")
	if len(refs) != 1 || refs[0].Line != 3 {
		t.Errorf("references to B = %+v", refs)
	}
	if refs[0].Path != "main.go" {
		t.Errorf("path should be workspace-relative, got %q", refs[0].Path)
	}
}
