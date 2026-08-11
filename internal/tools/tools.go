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

	"yagent/internal/index"
	"yagent/internal/llm"
	"yagent/internal/memory"
	"yagent/internal/skills"
	"yagent/internal/web"
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

// Options wires optional subsystems into the registry.
type Options struct {
	// Vectors + SessionID enable the semantic-memory tools (may be nil/empty).
	Vectors   *memory.VectorStore
	SessionID string
	// Skills enables the skills tools (may be nil).
	Skills *skills.Store
	// Index enables the codebase-index tools (may be nil).
	Index *index.Store
	// Web enables the M5 web tools (may be nil).
	Web *web.Client
	// Consult enables the `consult` advisor tool (may be nil).
	Consult *llm.Client
	// SkillsWriteApproval gates skill writes (stage instead of apply).
	SkillsWriteApproval bool
	// IndexProgress reports index_repo progress lines to the UI (optional).
	IndexProgress func(string)
}

// Registry holds the tool set bound to one workspace root.
type Registry struct {
	workspace string
	tools     map[string]Tool
	skills    *skills.Store
}

// NewRegistry builds the M2+ tool set scoped to workspace.
func NewRegistry(workspace string, opts Options) *Registry {
	r := &Registry{
		workspace: filepath.Clean(workspace),
		tools:     make(map[string]Tool),
		skills:    opts.Skills,
	}
	reg := map[string]Tool{
		"fs_read":       &fsReadTool{ws: r.workspace},
		"fs_write":      &fsWriteTool{ws: r.workspace},
		"fs_edit":       &fsEditTool{ws: r.workspace},
		"glob":          &globTool{ws: r.workspace},
		"grep":          &grepTool{ws: r.workspace},
		"shell_exec":    &shellExecTool{ws: r.workspace},
		"git_status":    &gitStatusTool{ws: r.workspace},
		"git_diff":      &gitDiffTool{ws: r.workspace},
		"git_log":       &gitLogTool{ws: r.workspace},
		"memory_save":   &memorySaveTool{vectors: opts.Vectors, sessionID: opts.SessionID},
		"memory_search": &memorySearchTool{vectors: opts.Vectors},
	}
	if opts.Skills != nil {
		reg["skills_list"] = &skillsListTool{store: opts.Skills}
		reg["skill_view"] = &skillViewTool{store: opts.Skills}
		reg["skill_manage"] = &skillManageTool{store: opts.Skills, writeApproval: opts.SkillsWriteApproval}
	}
	if opts.Index != nil {
		reg["index_repo"] = &indexRepoTool{store: opts.Index, onProgress: opts.IndexProgress}
		reg["index_search"] = &indexSearchTool{store: opts.Index}
	}
	if opts.Web != nil {
		reg["web_search"] = &webSearchTool{client: opts.Web}
		reg["web_fetch"] = &webFetchTool{client: opts.Web}
	}
	if opts.Consult != nil {
		reg["consult"] = &consultTool{client: opts.Consult}
	}
	for name, t := range reg {
		r.tools[name] = t
	}
	return r
}

// SetSkillsWriteApproval toggles the skill write gate at runtime (/skills
// approval on|off).
func (r *Registry) SetSkillsWriteApproval(on bool) {
	if t, ok := r.tools["skill_manage"].(*skillManageTool); ok {
		t.writeApproval = on
	}
}

// SetIndexProgress wires a progress sink for index_repo (set by the UI).
func (r *Registry) SetIndexProgress(fn func(string)) {
	if t, ok := r.tools["index_repo"].(*indexRepoTool); ok {
		t.onProgress = fn
	}
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

// SchemasFor returns the schemas of the named tools (used by the skills
// creation-opportunity pass, which offers only the skills tools).
func (r *Registry) SchemasFor(names []string) []llm.ToolSchema {
	out := make([]llm.ToolSchema, 0, len(names))
	for _, n := range names {
		if t, ok := r.tools[n]; ok {
			out = append(out, t.Schema())
		}
	}
	return out
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

func numProp(description string) map[string]any {
	return map[string]any{"type": "number", "description": description}
}

// decodeArgs strictly decodes the model's JSON arguments into v. Unknown
// fields and malformed JSON produce ValidationErrors the model can fix. Minor
// JSON slips (trailing commas, raw newlines inside strings) get one repair
// pass first so a small model doesn't burn a retry turn on them.
func decodeArgs(args json.RawMessage, v any) error {
	if len(bytes.TrimSpace(args)) == 0 {
		return validationErrorf("no arguments provided; pass a JSON object with the required fields")
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if repaired := repairJSON(args); string(repaired) != string(args) {
			dec2 := json.NewDecoder(bytes.NewReader(repaired))
			dec2.DisallowUnknownFields()
			if err2 := dec2.Decode(v); err2 == nil {
				return nil
			}
		}
		return validationErrorf("invalid arguments: %v; pass a JSON object matching the schema", err)
	}
	return nil
}

// repairJSON fixes the most common small-model JSON slips without changing
// semantics: trailing commas before } / ] and raw newlines/tabs inside string
// literals. It scans byte-by-byte, tracking string state.
func repairJSON(b []byte) []byte {
	out := make([]byte, 0, len(b))
	inString := false
	for i := 0; i < len(b); i++ {
		c := b[i]
		if inString {
			switch c {
			case '\\':
				out = append(out, c)
				if i+1 < len(b) {
					out = append(out, b[i+1])
					i++
				}
			case '"':
				out = append(out, c)
				inString = false
			case '\n':
				out = append(out, '\\', 'n')
			case '\r':
				// drop bare carriage returns
			case '\t':
				out = append(out, '\\', 't')
			default:
				out = append(out, c)
			}
			continue
		}
		switch c {
		case '"':
			inString = true
			out = append(out, c)
		case '}', ']':
			// drop a trailing comma (with whitespace) before this bracket
			n := len(out)
			for n > 0 && (out[n-1] == ' ' || out[n-1] == '\t' || out[n-1] == '\n' || out[n-1] == '\r') {
				n--
			}
			if n > 0 && out[n-1] == ',' {
				out = out[:n-1]
			}
			out = append(out, c)
		default:
			out = append(out, c)
		}
	}
	return out
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
