package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/Mechres/Yagent/internal/llm"
)

// ---------- code_environment ----------

// environmentTool audits the toolchain and build environment a small model
// might wrongly try to fix by editing source: installed compilers/interpreters,
// relevant env flags (CGO_ENABLED, CC), and native-dependency signals. A build
// failing from a missing tool is an environment problem, not a code problem —
// this tells the model that before it wastes turns editing Go source.
type environmentTool struct {
	ws string
}

var environmentSchema = fnSchema("code_environment", "audit the build environment: which compilers/interpreters are installed (go, gcc, rustc, node, python3, pkg-config, bwrap), the CGO_ENABLED/CC/GOFLAGS flags, and whether the workspace uses cgo/native bindings. Use it BEFORE trying to fix a build failure — if the error is a missing tool or a cgo issue, editing source will not help",
	map[string]any{}, []string{})

func (t *environmentTool) Schema() llm.ToolSchema { return environmentSchema }
func (t *environmentTool) Risk() RiskLevel        { return RiskReadOnly }

// envBinaries are the toolchain commands worth probing (name -> semantic label).
var envBinaries = []string{"go", "gcc", "cc", "clang", "rustc", "cargo", "node", "npm", "python3", "make", "cmake", "git", "pkg-config", "bwrap", "docker"}

func (t *environmentTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	if err := decodeArgs(raw, &struct{}{}); err != nil {
		return "", err
	}
	var b strings.Builder

	// Toolchain versions.
	b.WriteString("toolchain:\n")
	for _, bin := range envBinaries {
		path, err := exec.LookPath(bin)
		if err != nil {
			fmt.Fprintf(&b, "  %-12s MISSING\n", bin)
			continue
		}
		ver := shortVersion(bin, path)
		fmt.Fprintf(&b, "  %-12s %s\n", bin, ver)
	}

	// Env flags that affect builds.
	b.WriteString("env flags:\n")
	for _, k := range []string{"CGO_ENABLED", "CC", "CXX", "GOFLAGS", "GOOS", "GOARCH", "RUSTFLAGS", "PATH"} {
		if v := os.Getenv(k); v != "" {
			fmt.Fprintf(&b, "  %-12s %s\n", k, truncate(v, 120))
		}
	}
	fmt.Fprintf(&b, "  runtime      %s/%s (go %s)\n", runtime.GOOS, runtime.GOARCH, runtime.Version())

	// Native-binding signals in the workspace (cgo, ffi, node-gyp).
	if cgo, err := scanNativeBindings(t.ws); err == nil && len(cgo) > 0 {
		fmt.Fprintf(&b, "native bindings:\n")
		for _, f := range cgo {
			fmt.Fprintf(&b, "  %s\n", f)
		}
	} else {
		b.WriteString("native bindings: none detected\n")
	}

	return capResult(b.String(), maxResultBytes), nil
}

// shortVersion runs "<bin> --version" (or -V) and returns the first line.
func shortVersion(bin, path string) string {
	for _, flag := range []string{"--version", "-V", "version"} {
		out, err := exec.Command(path, flag).Output()
		if err != nil {
			continue
		}
		line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
		if line != "" {
			return truncate(line, 80)
		}
	}
	return "present"
}

// scanNativeBindings finds source files that signal cgo/FFI/native deps: Go's
// import "C" / #cgo, Rust's extern/ffi, C's #include, and node-gyp build files.
// Returns up to 5 rel paths.
func scanNativeBindings(ws string) ([]string, error) {
	var hits []string
	_ = filepath.WalkDir(ws, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || len(hits) >= 5 {
			return nil
		}
		if strings.Contains(filepath.Base(path), "node_modules") {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		need := false
		switch ext {
		case ".go":
			if data, err := os.ReadFile(path); err == nil {
				need = strings.Contains(string(data), `import "C"`) || strings.Contains(string(data), "#cgo")
			}
		case ".rs":
			if data, err := os.ReadFile(path); err == nil {
				need = strings.Contains(string(data), "extern \"C\"") || strings.Contains(string(data), "#[link")
			}
		case ".c", ".h", ".cc", ".cpp", ".hpp":
			need = true
		case ".js", ".ts":
			if strings.Contains(strings.ToLower(filepath.Base(path)), "binding") {
				need = true
			}
		}
		if need {
			if rel, err := filepath.Rel(ws, path); err == nil {
				hits = append(hits, filepath.ToSlash(rel))
			}
		}
		return nil
	})
	sort.Strings(hits)
	return hits, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
