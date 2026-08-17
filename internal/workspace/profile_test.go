package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeLookPath(available ...string) func(string) (string, error) {
	set := make(map[string]bool, len(available))
	for _, name := range available {
		set[name] = true
	}
	return func(name string) (string, error) {
		if set[name] {
			return "/bin/" + name, nil
		}
		return "", fmt.Errorf("%s not found", name)
	}
}

func writeMarker(t *testing.T, root, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDetectGreenfieldKeepsFrameworkChoiceUnknown(t *testing.T) {
	root := t.TempDir()
	p := detect(root, fakeLookPath("go", "node", "git", "bwrap"))
	if !p.Greenfield() || p.VerificationReady {
		t.Fatalf("profile = %#v, want a greenfield profile without verification", p)
	}
	context := p.Context()
	for _, want := range []string{"greenfield", "available commands: go, node, git, bwrap", "Ask with clarify"} {
		if !strings.Contains(context, want) {
			t.Errorf("context %q missing %q", context, want)
		}
	}
	if !p.SuppressVerificationTools() {
		t.Error("greenfield profile should suppress project verification schemas")
	}
}

func TestDetectRecognizedProjectDistinguishesMissingToolchain(t *testing.T) {
	root := t.TempDir()
	writeMarker(t, root, "go.mod")
	p := detect(root, fakeLookPath("git"))
	if p.Kind != "Go" || p.Greenfield() || p.VerificationReady {
		t.Fatalf("profile = %#v, want Go with unavailable verification", p)
	}
	for _, want := range []string{"verification: unavailable (missing: go)", "missing prerequisite"} {
		if !strings.Contains(p.Context(), want) {
			t.Errorf("context = %q, want %q", p.Context(), want)
		}
	}
	if !p.SuppressVerificationTools() {
		t.Error("missing Go toolchain should suppress project verification schemas")
	}
}

func TestDetectRecognizedProjectEnablesVerification(t *testing.T) {
	root := t.TempDir()
	writeMarker(t, root, "package.json")
	writeMarker(t, root, "tsconfig.json")
	p := detect(root, fakeLookPath("npx", "node", "git"))
	if p.Kind != "TypeScript" || !p.VerificationReady || p.SuppressVerificationTools() {
		t.Fatalf("profile = %#v, want verified TypeScript project", p)
	}
	if !strings.Contains(p.Context(), "verification: available") {
		t.Errorf("context = %q", p.Context())
	}
}
