// Package workspace detects a small, deterministic capability profile for a
// workspace. It is deliberately manifest- and toolchain-based: it never asks
// the model to guess a framework before deciding which verification tools are
// worth offering.
package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Profile is the compact workspace state given to the agent. A project can be
// recognized even when its required toolchain is missing; that distinction is
// important because the model must explain the missing prerequisite rather
// than attempting source edits that cannot fix it.
type Profile struct {
	Kind              string
	Markers           []string
	Available         []string
	Missing           []string
	VerificationReady bool
}

// Detect inspects root and the relevant locally installed commands.
func Detect(root string) Profile {
	return detect(root, exec.LookPath)
}

func detect(root string, lookPath func(string) (string, error)) Profile {
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	has := func(name string) bool {
		_, err := os.Stat(filepath.Join(root, name))
		return err == nil
	}

	p := Profile{}
	switch {
	case has("go.mod"):
		p.Kind, p.Markers = "Go", []string{"go.mod"}
		p.VerificationReady = commandAvailable(lookPath, "go")
	case has("Cargo.toml"):
		p.Kind, p.Markers = "Rust", []string{"Cargo.toml"}
		p.VerificationReady = commandAvailable(lookPath, "cargo")
	case has("package.json") && has("tsconfig.json"):
		p.Kind, p.Markers = "TypeScript", []string{"package.json", "tsconfig.json"}
		p.VerificationReady = commandAvailable(lookPath, "npx")
	case has("package.json"):
		p.Kind, p.Markers = "JavaScript", []string{"package.json"}
		p.VerificationReady = commandAvailable(lookPath, "node") || commandAvailable(lookPath, "npx")
	case has("pyproject.toml") || has("requirements.txt") || has("setup.py"):
		p.Kind = "Python"
		for _, marker := range []string{"pyproject.toml", "requirements.txt", "setup.py"} {
			if has(marker) {
				p.Markers = append(p.Markers, marker)
			}
		}
		p.VerificationReady = commandAvailable(lookPath, "python3") || commandAvailable(lookPath, "ruff")
	case has("CMakeLists.txt") || has("Makefile") || has("makefile"):
		p.Kind = "C/C++"
		for _, marker := range []string{"CMakeLists.txt", "Makefile", "makefile"} {
			if has(marker) {
				p.Markers = append(p.Markers, marker)
			}
		}
		p.VerificationReady = commandAvailable(lookPath, "cmake") || commandAvailable(lookPath, "make") || commandAvailable(lookPath, "cc") || commandAvailable(lookPath, "gcc") || commandAvailable(lookPath, "g++")
	default:
		p.Kind = "greenfield"
	}

	for _, cmd := range []string{"go", "cargo", "node", "npx", "python3", "ruff", "cc", "gcc", "g++", "cmake", "make", "git", "bwrap"} {
		if commandAvailable(lookPath, cmd) {
			p.Available = append(p.Available, cmd)
		} else {
			p.Missing = append(p.Missing, cmd)
		}
	}
	return p
}

func commandAvailable(lookPath func(string) (string, error), name string) bool {
	_, err := lookPath(name)
	return err == nil
}

// Greenfield reports whether no supported project manifest is present.
func (p Profile) Greenfield() bool { return p.Kind == "greenfield" }

// Context is a compact, model-facing profile. It deliberately distinguishes
// unknown framework choice from a missing executable so an empty folder is a
// supported starting point, not an error state.
func (p Profile) Context() string {
	if p.Kind == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("[WORKSPACE PROFILE]\n")
	if p.Greenfield() {
		b.WriteString("state: greenfield (no supported project manifest detected)\n")
	} else {
		b.WriteString("project: " + p.Kind)
		if len(p.Markers) > 0 {
			b.WriteString(" (" + strings.Join(p.Markers, ", ") + ")")
		}
		b.WriteByte('\n')
		if p.VerificationReady {
			b.WriteString("verification: available\n")
		} else {
			b.WriteString("verification: unavailable (missing: " + p.verificationPrerequisite() + ")\n")
		}
	}
	if len(p.Available) > 0 {
		b.WriteString("available commands: " + strings.Join(p.Available, ", ") + "\n")
	}
	if p.Greenfield() {
		b.WriteString("guidance: no language or framework is selected. Ask with clarify, or offer a small scaffold before writing. Re-check the workspace after creating its manifest.\n")
	} else if !p.VerificationReady {
		b.WriteString("guidance: explain the missing prerequisite; do not try to repair a missing toolchain by editing source.\n")
	} else {
		b.WriteString("guidance: use the project-appropriate diagnostics and tests after edits.\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// SuppressVerificationTools reports whether fixed project verification tools
// would only fail or report "no project". The registry still resolves them if
// a model explicitly calls one; this only removes unhelpful schema noise.
func (p Profile) SuppressVerificationTools() bool {
	return p.Greenfield() || !p.VerificationReady
}

func (p Profile) verificationPrerequisite() string {
	switch p.Kind {
	case "Go":
		return "go"
	case "Rust":
		return "cargo"
	case "TypeScript":
		return "npx"
	case "JavaScript":
		return "node or npx"
	case "Python":
		return "python3 or ruff"
	case "C/C++":
		return "cmake, make, or a C/C++ compiler"
	default:
		return "required toolchain"
	}
}
