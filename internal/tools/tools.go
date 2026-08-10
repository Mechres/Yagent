// Package tools implements the agent's tools: typed schemas, workspace
// scoping, risk levels, and the registry. Contract: a tool's Execute returns
// a non-nil error ONLY for argument-validation failures (before any side
// effect); execution failures are returned as result text so the model can
// self-correct from them.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"yagent/internal/llm"
)

// RiskLevel classifies a tool's side effects; Write/Destructive tools go
// through the user approval gate.
type RiskLevel int

const (
	RiskReadOnly RiskLevel = iota
	RiskWrite
	RiskDestructive
)

func (r RiskLevel) String() string {
	switch r {
	case RiskReadOnly:
		return "read-only"
	case RiskWrite:
		return "write"
	case RiskDestructive:
		return "destructive"
	}
	return "unknown"
}

// ValidationError marks argument-validation failures; the agent counts these
// toward the per-call retry cap.
type ValidationError struct{ msg string }

func (e *ValidationError) Error() string { return e.msg }

func validationErrorf(format string, args ...any) error {
	return &ValidationError{msg: fmt.Sprintf(format, args...)}
}

// Tool is one executable capability.
type Tool interface {
	Schema() llm.ToolSchema
	Risk() RiskLevel
	Execute(ctx context.Context, args json.RawMessage) (string, error)
}

// Registry holds the tool set bound to one workspace root.
type Registry struct {
	workspace string
	tools     map[string]Tool
}

// NewRegistry builds the M2 tool set scoped to workspace.
func NewRegistry(workspace string) *Registry {
	r := &Registry{
		workspace: filepath.Clean(workspace),
		tools:     make(map[string]Tool),
	}
	reg := map[string]Tool{
		"fs_read":    &fsReadTool{ws: r.workspace},
		"fs_write":   &fsWriteTool{ws: r.workspace},
		"fs_edit":    &fsEditTool{ws: r.workspace},
		"glob":       &globTool{ws: r.workspace},
		"grep":       &grepTool{ws: r.workspace},
		"shell_exec": &shellExecTool{ws: r.workspace},
		"git_status": &gitStatusTool{ws: r.workspace},
		"git_diff":   &gitDiffTool{ws: r.workspace},
		"git_log":    &gitLogTool{ws: r.workspace},
	}
	for name, t := range reg {
		r.tools[name] = t
	}
	return r
}

// Get returns a tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Names returns all tool names, sorted for determinism.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Schemas returns all tool schemas, sorted by name.
func (r *Registry) Schemas() []llm.ToolSchema {
	schemas := make([]llm.ToolSchema, 0, len(r.tools))
	for _, n := range r.Names() {
		schemas = append(schemas, r.tools[n].Schema())
	}
	return schemas
}

// fnSchema builds a compact OpenAI function schema. properties and required
// are normalized to non-null values: llama.cpp rejects "required": null when
// building a tool-call grammar.
func fnSchema(name, description string, properties map[string]any, required []string) llm.ToolSchema {
	if properties == nil {
		properties = map[string]any{}
	}
	if required == nil {
		required = []string{}
	}
	s := llm.ToolSchema{Type: "function"}
	s.Function.Name = name
	s.Function.Description = description
	s.Function.Parameters = map[string]any{
		"type":       "object",
		"properties": properties,
		"required":   required,
	}
	return s
}

func strProp(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func intProp(description string) map[string]any {
	return map[string]any{"type": "integer", "description": description}
}

// decodeArgs strictly decodes the model's JSON arguments into v. Unknown
// fields and malformed JSON produce ValidationErrors the model can fix.
func decodeArgs(args json.RawMessage, v any) error {
	if len(bytes.TrimSpace(args)) == 0 {
		return validationErrorf("no arguments provided; pass a JSON object with the required fields")
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return validationErrorf("invalid arguments: %v; pass a JSON object matching the schema", err)
	}
	return nil
}

// resolvePath maps a model-supplied path onto the workspace, rejecting any
// path that escapes the workspace root.
func resolvePath(workspace, p string) (string, error) {
	if p == "" {
		return "", validationErrorf(`argument "path" is required`)
	}
	if filepath.IsAbs(p) {
		return "", validationErrorf("absolute path %q not allowed; use a path relative to the workspace root", p)
	}
	abs := filepath.Clean(filepath.Join(workspace, p))
	root := filepath.Clean(workspace)
	if abs != root && !strings.HasPrefix(abs, root+string(filepath.Separator)) {
		return "", validationErrorf("path %q escapes the workspace root %q", p, root)
	}
	return abs, nil
}

// maxResultBytes is the default per-tool result cap.
const maxResultBytes = 32 << 10

// capResult truncates a tool result to maxBytes with an explicit marker.
func capResult(s string, maxBytes int) string {
	if maxBytes <= 0 {
		maxBytes = maxResultBytes
	}
	if len(s) <= maxBytes {
		return s
	}
	return s[:maxBytes] + fmt.Sprintf("\n... truncated (%d bytes omitted)", len(s)-maxBytes)
}
