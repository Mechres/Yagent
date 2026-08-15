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
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Mechres/Yagent/internal/index"
	"github.com/Mechres/Yagent/internal/jobs"
	"github.com/Mechres/Yagent/internal/llm"
	"github.com/Mechres/Yagent/internal/mcp"
	"github.com/Mechres/Yagent/internal/memory"
	"github.com/Mechres/Yagent/internal/skills"
	"github.com/Mechres/Yagent/internal/undo"
	"github.com/Mechres/Yagent/internal/web"
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

// errorClass renders a tool error with a machine-readable class marker the
// model can act on programmatically (Luna review #8 / Nemotron #4): a stable
// class name, whether retrying makes sense, and suggested next tools.
func errorClass(class string, retryable bool, suggest []string, msg string) string {
	var b strings.Builder
	b.WriteString("error: ")
	b.WriteString(msg)
	fmt.Fprintf(&b, " [class=%s retryable=%t", class, retryable)
	if len(suggest) > 0 {
		fmt.Fprintf(&b, " suggest=%s", strings.Join(suggest, ","))
	}
	b.WriteString("]")
	return b.String()
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
	// Vectors + SessionID enable the semantic-memory tools (may be nil/empty);
	// ProjectVectors is a repo-shared store the tools also search/write.
	Vectors        *memory.VectorStore
	ProjectVectors *memory.VectorStore
	SessionID      string
	// Skills enables the skills tools (may be nil).
	Skills *skills.Store
	// Index enables the codebase-index tools (may be nil).
	Index *index.Store
	// Web enables the M5 web tools (may be nil).
	Web *web.Client
	// Consult enables the `consult` advisor tool (may be nil).
	Consult *llm.Client
	// Undo records file writes for /undo (may be nil).
	Undo *undo.Buffer
	// ShellSandbox wraps shell_exec in bubblewrap when set to "bwrap".
	ShellSandbox string
	// MCP attaches Model Context Protocol servers; each advertised tool is
	// registered under the server-prefixed name (<server>_<tool>).
	MCP []*mcp.Client
	// Hooks are lifecycle hooks (Hermes P0): a command run before ("pre") or
	// after ("post") a matching tool executes.
	Hooks []Hook
	// ReadOnly restricts the registry to read-only tools (used by subagents).
	ReadOnly bool
	// Subagent delegates a task to an isolated child agent (M7 v1). The
	// tools slice scopes the child registry (M7 beyond v2); nil = full set.
	// The role is the resolved preset profile (P2), zero value when none.
	Subagent func(ctx context.Context, task, workspace string, tools []string, role SubagentRole) (string, error)
	// AskUser prompts the user with a question and optional choices and returns
	// the answer (wired by the UI); enables the clarify and plan tools. Nil
	// disables both.
	AskUser askUserFunc
	// Jobs enables background-process tools (may be nil).
	Jobs *jobs.Registry
	// ConsultCmd is an installed terminal AI app used as the advisor, e.g.
	// ["claude", "-p"] (the prompt is appended as the final argument).
	ConsultCmd []string
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
	hooks     *Hooks
}

// skillsDirs joins the skills store's SKILL.md roots ("|"-separated) so the fs
// write tools can recognize model writes to the skills store and route them
// through the skills gate instead of the generic y/n prompt. Empty when no
// skills store is configured.
func skillsDirs(sk *skills.Store) string {
	if sk == nil {
		return ""
	}
	return strings.Join(sk.SkillDirs(), "|")
}

// NewRegistry builds the M2+ tool set scoped to workspace.
func NewRegistry(workspace string, opts Options) *Registry {
	r := &Registry{
		workspace: filepath.Clean(workspace),
		tools:     make(map[string]Tool),
		skills:    opts.Skills,
		hooks:     NewHooks(opts.Hooks),
	}
	reg := map[string]Tool{
		"fs_read":               &fsReadTool{ws: r.workspace},
		"fs_write":              &fsWriteTool{ws: r.workspace, undo: opts.Undo, skillsDir: skillsDirs(opts.Skills)},
		"fs_edit":               &fsEditTool{ws: r.workspace, undo: opts.Undo, skillsDir: skillsDirs(opts.Skills)},
		"fs_patch":              &fsPatchTool{ws: r.workspace, undo: opts.Undo},
		"fs_refactor":           &refactorTool{ws: r.workspace, undo: opts.Undo},
		"code_outline":          &codeOutlineTool{ws: r.workspace},
		"code_slice":            &codeSliceTool{ws: r.workspace},
		"code_topology":         &codeTopologyTool{ws: r.workspace},
		"glob":                  &globTool{ws: r.workspace},
		"grep":                  &grepTool{ws: r.workspace},
		"workspace_diagnostics": &diagnosticsTool{ws: r.workspace},
		"test_runner":           &testRunnerTool{ws: r.workspace},
		"runtime_smoke":         &smokeTool{ws: r.workspace},
		"code_environment":      &environmentTool{ws: r.workspace},
		"shell_exec":            &shellExecTool{ws: r.workspace, sandbox: opts.ShellSandbox},
		"git_status":            &gitStatusTool{ws: r.workspace},
		"git_diff":              &gitDiffTool{ws: r.workspace},
		"git_log":               &gitLogTool{ws: r.workspace},
		"memory_save":           &memorySaveTool{vectors: opts.Vectors, projectVectors: opts.ProjectVectors, sessionID: opts.SessionID},
		"memory_search":         &memorySearchTool{vectors: opts.Vectors, projectVectors: opts.ProjectVectors},
	}
	if opts.Skills != nil {
		reg["skills_list"] = &skillsListTool{store: opts.Skills}
		reg["skill_view"] = &skillViewTool{store: opts.Skills}
		reg["skill_manage"] = &skillManageTool{store: opts.Skills, writeApproval: opts.SkillsWriteApproval}
	}
	if opts.Index != nil {
		reg["index_repo"] = &indexRepoTool{store: opts.Index, onProgress: opts.IndexProgress}
		reg["index_search"] = &indexSearchTool{store: opts.Index}
		reg["code_references"] = &codeReferencesTool{store: opts.Index}
		reg["code_impact"] = &codeImpactTool{store: opts.Index, ws: r.workspace}
		reg["code_unused"] = &codeUnusedTool{store: opts.Index}
	}
	if opts.Web != nil {
		reg["web_search"] = &webSearchTool{client: opts.Web}
		reg["web_fetch"] = &webFetchTool{client: opts.Web}
	}
	if opts.Consult != nil || len(opts.ConsultCmd) > 0 {
		reg["consult"] = &consultTool{client: opts.Consult, cmd: opts.ConsultCmd}
	}
	if opts.Subagent != nil {
		reg["subagent"] = &subagentTool{ws: r.workspace, run: opts.Subagent}
	}
	if opts.AskUser != nil {
		reg["clarify"] = &clarifyTool{ask: opts.AskUser}
		reg["plan"] = &planTool{ask: opts.AskUser}
	}
	if opts.Jobs != nil {
		reg["shell_bg"] = &shellBgTool{jobs: opts.Jobs, sandbox: opts.ShellSandbox, ws: r.workspace}
		reg["shell_logs"] = &shellLogsTool{jobs: opts.Jobs}
		reg["shell_kill"] = &shellKillTool{jobs: opts.Jobs}
	}
	// MCP servers: each advertised tool is registered as <server>_<tool>. MCP
	// tools are treated as read-only for the approval gate (their own server
	// enforces its policy; blocking every call would defeat the point), but a
	// server's destructiveHint could later gate it.
	for _, client := range opts.MCP {
		for _, t := range client.Tools() {
			reg[mcpToolName(client.Name(), t.Name)] = &mcpTool{client: client, tool: t}
		}
	}
	// Scratchpad: available to everyone, including read-only subagents (its
	// writes are strictly confined to .yagent/scratch/).
	reg["scratch_write"] = &scratchWriteTool{ws: r.workspace}
	reg["scratch_read"] = &scratchReadTool{ws: r.workspace}
	for name, t := range reg {
		if opts.ReadOnly && t.Risk() != RiskReadOnly && name != "scratch_write" {
			continue // subagents get read-only tools (plus the confined scratch_write)
		}
		r.tools[name] = t
	}
	return r
}

// SetSubagent wires the subagent delegate at runtime. The callback receives the
// tool subset and the resolved role preset (P2), so the caller can scope the
// child registry and sampling accordingly.
func (r *Registry) SetSubagent(fn func(ctx context.Context, task, workspace string, tools []string, role SubagentRole) (string, error)) {
	if t, ok := r.tools["subagent"].(*subagentTool); ok {
		t.run = fn
	} else if fn != nil {
		r.tools["subagent"] = &subagentTool{ws: r.workspace, run: fn}
	}
}

// SetAskUser wires the user-prompt callback at runtime (after the UI's reader/
// writer exist) and registers the clarify/plan tools when they weren't at
// construction.
func (r *Registry) SetAskUser(fn askUserFunc) {
	if fn == nil {
		return
	}
	if t, ok := r.tools["clarify"].(*clarifyTool); ok {
		t.ask = fn
	} else {
		r.tools["clarify"] = &clarifyTool{ask: fn}
	}
	if t, ok := r.tools["plan"].(*planTool); ok {
		t.ask = fn
	} else {
		r.tools["plan"] = &planTool{ask: fn}
	}
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

// Restrict returns a shallow copy of the registry exposing only the named
// tools, validating each name exists. Used to scope a subagent to a tool
// subset (M7 beyond v2): requested tools that aren't in this registry — e.g. a
// destructive tool on a read-only subagent — produce an error the model can
// read and fix.
func (r *Registry) Restrict(names []string) (*Registry, error) {
	nr := &Registry{workspace: r.workspace, tools: make(map[string]Tool, len(names))}
	for _, n := range names {
		t, ok := r.tools[n]
		if !ok {
			return nil, fmt.Errorf("tool %q is not available to subagents (available: %s)", n, strings.Join(r.Names(), ", "))
		}
		nr.tools[n] = t
	}
	return nr, nil
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

// ExecuteWithHooks runs a tool with the configured lifecycle hooks: pre-hooks
// (a non-zero exit vetoes the call), then the tool, then post-hooks. Returns
// the tool's result text and a validation-style error.
func (r *Registry) ExecuteWithHooks(ctx context.Context, name string, args json.RawMessage) (string, error) {
	if err := r.hooks.RunPre(name, args); err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	tool, ok := r.tools[name]
	if !ok {
		return fmt.Sprintf("error: unknown tool %q, available: %s", name, strings.Join(r.Names(), ", ")), nil
	}
	result, err := tool.Execute(ctx, args)
	r.hooks.RunPost(name, args)
	return result, err
}

// MCPToolNames returns the names of tools registered from MCP servers (the
// server-prefixed set). The agent always offers them so an attached server's
// capabilities are usable without a domain signal.
func (r *Registry) MCPToolNames() []string {
	var out []string
	for n, t := range r.tools {
		if _, ok := t.(*mcpTool); ok {
			out = append(out, n)
		}
	}
	return out
}

// MCPToolNamesForSignal returns the MCP tool names to OFFER for a request: only
// the tools whose server is signaled in the input (its name appears in the text
// — e.g. asking about "gitinfo" or "context7") or that the model already used
// this turn. A large MCP server otherwise re-floods every request with all of
// its schemas, undoing dynamic filtering and confusing a 7B-9B model (GPT
// sol #7). Tools the model calls anyway still resolve at dispatch — this only
// trims what the model *sees*.
func (r *Registry) MCPToolNamesForSignal(input string, used map[string]bool) []string {
	lower := strings.ToLower(input)
	var out []string
	for n, t := range r.tools {
		mt, ok := t.(*mcpTool)
		if !ok {
			continue
		}
		if used[n] {
			out = append(out, n)
			continue
		}
		if server := strings.ToLower(mt.client.Name()); server != "" && strings.Contains(lower, server) {
			out = append(out, n)
		}
	}
	return out
}

// SchemasForReadOnly returns the schemas of every read-only tool in the
// registry, plus the named extras (plan/consult) — used by read-only plan mode
// (Hermes P0): the model may explore but not mutate until the plan is approved.
func (r *Registry) SchemasForReadOnly(extras ...string) []llm.ToolSchema {
	names := append([]string(nil), extras...)
	for n, t := range r.tools {
		if t.Risk() == RiskReadOnly && !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	return r.SchemasFor(names)
}

// Hook is one lifecycle hook: a command run before ("pre") or after ("post") a
// matching tool executes. Tool "*" matches every tool. The hook receives the
// tool name via YAGENT_TOOL and the raw JSON args via YAGENT_ARGS. A pre-hook
// with a non-zero exit vetoes the tool call.
type Hook struct {
	When    string   // "pre" or "post"
	Tool    string   // tool name, or "*"
	Command []string // argv to run
}

// Hooks holds the configured hook set and runs the matching ones.
type Hooks struct {
	hooks []Hook
}

// NewHooks wraps the configured hooks.
func NewHooks(hooks []Hook) *Hooks { return &Hooks{hooks: hooks} }

// RunPre executes every pre-hook matching tool. Returns an error (the first
// non-zero pre-hook) to veto the call.
func (h *Hooks) RunPre(toolName string, args json.RawMessage) error {
	return h.run("pre", toolName, args, true)
}

// RunPost executes every post-hook matching tool (best-effort).
func (h *Hooks) RunPost(toolName string, args json.RawMessage) {
	_ = h.run("post", toolName, args, false)
}

func (h *Hooks) run(when, toolName string, args json.RawMessage, veto bool) error {
	if h == nil {
		return nil
	}
	for _, hook := range h.hooks {
		if hook.When != when {
			continue
		}
		if hook.Tool != "*" && hook.Tool != toolName {
			continue
		}
		if len(hook.Command) == 0 {
			continue
		}
		cmd := exec.Command(hook.Command[0], hook.Command[1:]...)
		cmd.Env = append(os.Environ(), "YAGENT_TOOL="+toolName, "YAGENT_ARGS="+string(args))
		if err := cmd.Run(); err != nil && veto {
			return fmt.Errorf("pre-hook for %s vetoed: %v", toolName, err)
		}
	}
	return nil
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

func arrayProp(description string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": description}
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
// pass first, then a fuzzy key-alias pass, so a small model doesn't burn a
// retry turn on {"filename": "x"} instead of {"path": "x"}.
func decodeArgs(args json.RawMessage, v any) error {
	if len(bytes.TrimSpace(args)) == 0 {
		return validationErrorf("no arguments provided; pass a JSON object with the required fields")
	}
	if string(bytes.TrimSpace(args)) == llm.TruncatedArgsMarker {
		return validationErrorf("tool-call arguments were truncated (incomplete JSON) — re-emit the full tool call with all required fields")
	}
	if err := strictDecode(args, v); err == nil {
		return nil
	}
	if repaired := repairJSON(args); string(repaired) != string(args) {
		if err := strictDecode(repaired, v); err == nil {
			return nil
		}
		args = repaired
	}
	if aliased := aliasKeys(args, v); aliased != nil {
		if err := strictDecode(aliased, v); err == nil {
			return nil
		}
	}
	if isTruncatedJSON(args) {
		// A stream cut mid-tool-call (small models near the context limit):
		// tell the model to re-emit the full call instead of chasing a syntax
		// error it can't see (Luna review #2).
		return validationErrorf("tool-call arguments were truncated (incomplete JSON) — re-emit the full tool call with all required fields")
	}
	return validationErrorf("invalid arguments: unknown or malformed fields; pass a JSON object matching the tool schema")
}

// isTruncatedJSON reports whether the raw arguments are structurally
// incomplete: an unclosed string or more open braces/brackets than close.
func isTruncatedJSON(args []byte) bool {
	depth := 0
	inString := false
	escaped := false
	for _, c := range args {
		if inString {
			if escaped {
				escaped = false
				continue
			}
			switch c {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		}
	}
	return depth > 0 || inString
}

func strictDecode(args json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return validationErrorf("invalid arguments: %v; pass a JSON object matching the schema", err)
	}
	return nil
}

// aliasKeys rewrites an arguments object, mapping unknown keys onto the closest
// schema field by synonym or Levenshtein distance ({"filename":"main.go"} →
// {"path":"main.go"}). Returns nil when nothing was remapped. The model sees
// the corrected arguments in the next history turn, so it learns the real
// names.
func aliasKeys(args json.RawMessage, v any) json.RawMessage {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(args, &m); err != nil || len(m) == 0 {
		return nil
	}
	fields := jsonFieldNames(v)
	if len(fields) == 0 {
		return nil
	}
	changed := false
	for key, val := range m {
		if _, ok := fields[key]; ok {
			continue
		}
		alias, ok := fuzzyFieldName(key, fields)
		if !ok {
			continue
		}
		if _, taken := m[alias]; taken {
			// canonical key already present; the alias is redundant
			delete(m, key)
			changed = true
			continue
		}
		m[alias] = val
		delete(m, key)
		changed = true
	}
	if !changed {
		return nil
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return out
}

// jsonFieldNames returns the JSON field names of a flat struct (json tags).
func jsonFieldNames(v any) map[string]bool {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Ptr || rv.IsNil() {
		return nil
	}
	rt := rv.Elem().Type()
	if rt.Kind() != reflect.Struct {
		return nil
	}
	names := make(map[string]bool, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		names[name] = true
	}
	return names
}

// fuzzyFieldName maps an unknown key to the closest schema field using a small
// synonym table first, then Levenshtein distance (allowing one edit per ~3
// characters).
func fuzzyFieldName(key string, fields map[string]bool) (string, bool) {
	lk := strings.ToLower(key)
	// synonyms: only applied when the target field actually exists
	for _, syn := range []struct{ alias, canonical string }{
		{"filename", "path"}, {"file", "path"}, {"dir", "path"}, {"folder", "path"},
		{"filepath", "path"}, {"cmd", "command"}, {"shell", "command"},
		{"regex", "pattern"}, {"content", "body"}, {"text", "content"},
		{"output", "content"}, {"timeout", "timeout_sec"},
	} {
		if lk == syn.alias && fields[syn.canonical] {
			return syn.canonical, true
		}
	}
	// Levenshtein against each candidate field
	best, bestD := "", 0
	for f := range fields {
		d := levenshtein(lk, strings.ToLower(f))
		limit := 1 + len(f)/3
		if d <= limit && (best == "" || d < bestD) {
			best, bestD = f, d
		}
	}
	if best != "" && bestD > 0 {
		return best, true
	}
	return "", false
}

// levenshtein computes the edit distance between two strings.
func levenshtein(a, b string) int {
	la, lb := len(a), len(b)
	prev := make([]int, lb+1)
	cur := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		cur[0] = i
		for j := 1; j <= lb; j++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}
			cur[j] = min3(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[lb]
}

func min3(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
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
// path that escapes the workspace root. It also resolves symlinks: a link
// inside the workspace may point outside it (e.g. one checked into a cloned
// repo), and the fs tools would happily follow it — so the deepest existing
// ancestor is resolved and its containment re-checked before any fs op runs.
func resolvePath(workspace, p string) (string, error) {
	if p == "" {
		return "", validationErrorf(`argument "path" is required`)
	}
	p = sanitizePathArg(workspace, p)
	// Absolute paths are accepted when they stay inside the workspace: local
	// models habitually emit them, and rejecting outright derails the loop.
	// Containment below handles the safety either way.
	abs := p
	if !filepath.IsAbs(p) {
		abs = filepath.Join(workspace, p)
	}
	abs = filepath.Clean(abs)
	root := filepath.Clean(workspace)
	if err := ensureContained(root, abs); err != nil {
		return "", err
	}
	resolved, err := ResolveSymlinks(abs)
	if err != nil {
		return "", err
	}
	if err := ensureContained(root, resolved); err != nil {
		return "", validationErrorf("path %q resolves outside the workspace root %q (symlink?)", p, root)
	}
	// Return the resolved path so reads/writes hit the verified real location,
	// not a link that could be swapped after the check.
	return resolved, nil
}

// sanitizePathArg cleans a small-model path slip before resolution (AGY #4):
//   - trims wrapping single/double quotes ("'main.go'" -> main.go)
//   - normalizes Windows backslashes to the platform separator (pkg\config.go)
//   - strips a leading "<workspace-name>/" prefix the model copied from a
//     trace or its own assumptions
//
// It never fabricates a path — containment checks still apply afterwards.
func sanitizePathArg(workspace, p string) string {
	p = strings.TrimSpace(p)
	p = strings.Trim(p, `"'`)
	// Windows-style separators: normalize to the local separator.
	if filepath.Separator != '\\' {
		p = strings.ReplaceAll(p, `\`, "/")
	}
	// A leading "<workspace basename>/..." prefix is a common slip; strip it.
	if base := filepath.Base(workspace); base != "" && base != "/" && base != "." && strings.HasPrefix(p, base+"/") {
		p = p[len(base)+1:]
	}
	return p
}

// ensureContained reports an error when abs escapes root.
func ensureContained(root, abs string) error {
	if abs != root && !strings.HasPrefix(abs, root+string(filepath.Separator)) {
		return validationErrorf("path %q escapes the workspace root %q", abs, root)
	}
	return nil
}

// ResolveSymlinks resolves symlinks along the deepest existing ancestor of
// abs, re-appending the remaining tail. This handles reads of existing files
// as well as writes to not-yet-created paths (where only the parent, or an
// ancestor of it, exists). Exported so the TUI's approval preview resolves
// paths with the same rules as the tools.
func ResolveSymlinks(abs string) (string, error) {
	clean, err := filepath.Abs(abs)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		return resolved, nil
	}
	var tail []string
	cur := filepath.Clean(clean)
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return resolved, nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, nil // nothing above resolves; fall back to the cleaned path
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
	}
}

// maxResultBytes is the default per-tool result cap.
const maxResultBytes = 32 << 10

// capResult truncates a tool result to maxBytes with an explicit marker.
func capResult(s string, maxBytes int) string {
	if maxBytes <= 0 {
		maxBytes = maxResultBytes
	}
	// Collapse repeated runs of identical lines first: build/test logs spam
	// the same line (repeating errors, progress ticks) and eat the budget.
	compact := compactLines(s)
	// Then, if the output is an error cascade (a single root cause producing
	// dozens of derived compiler/linter errors), group by signature so the
	// model sees the top few root causes instead of 200 lines of noise.
	if grouped := groupErrorCascade(compact); grouped != "" {
		compact = grouped
	}
	if len(compact) <= maxBytes {
		return compact
	}
	return compact[:maxBytes] + fmt.Sprintf("\n... truncated (%d bytes omitted)", len(compact)-maxBytes)
}

// offloadResult is capResult with a scratchpad escape hatch (deepseek review
// #4): when the output exceeds maxBytes, the full result is written to
// <ws>/.yagent/scratch/tool-output-<n>.txt and the return keeps the top 15
// lines (still capped at maxBytes) plus a pointer, so the model can fs_read the
// exact region without the data being lost. Small outputs pass through
// untouched. Falls back to plain capResult when ws is empty or the write fails.
func offloadResult(ws, s string, maxBytes int) string {
	if ws == "" || len(s) <= maxBytes {
		return capResult(s, maxBytes)
	}
	dir := filepath.Join(ws, scratchRoot)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return capResult(s, maxBytes)
	}
	name := fmt.Sprintf("tool-output-%d.txt", time.Now().UnixNano())
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		return capResult(s, maxBytes)
	}
	lines := strings.Split(s, "\n")
	keep := lines
	if len(keep) > 15 {
		keep = keep[:15]
	}
	head := strings.Join(keep, "\n")
	if len(head) > maxBytes {
		head = head[:maxBytes]
	}
	return head + fmt.Sprintf("\n\n[full output (%d bytes) saved to .yagent/scratch/%s — use fs_read on that file with offset/limit to inspect the rest]", len(s), name)
}

// maxDistinctErrors is how many distinct root causes the cascade summarizer
// keeps before folding the rest into a count.
const maxDistinctErrors = 3

// errorLocRe matches a compiler/linter error line that starts with a
// path:line(:col): prefix (go vet/compile, tsc, eslint, rustc, …).
var errorLocRe = regexp.MustCompile(`^[A-Za-z0-9_./\\-]+:(\d+)(?::(\d+))?\s*:\s*(.*)$`)

// errorSig strips location prefixes and trailing "at …" suffixes so every
// instance of the same root cause normalizes to one signature.
func errorSig(line string) string {
	m := errorLocRe.FindStringSubmatch(line)
	if m == nil {
		return ""
	}
	msg := m[3]
	msg = regexp.MustCompile(`(?:\s+at\s+[^ ]+:\d+(?::\d+)?)+$`).ReplaceAllString(msg, "")
	return strings.TrimSpace(msg)
}

// groupErrorCascade summarizes an error-dominated output: the top
// maxDistinctErrors root causes (by error signature) are kept with their
// first precise path:line:col pointer, and everything else is folded into a
// count. Returns "" when the output is not error-dominated, so normal tool
// results pass through untouched.
func groupErrorCascade(s string) string {
	lines := strings.Split(s, "\n")
	var errLines, other []string
	for _, ln := range lines {
		if errorSig(ln) != "" {
			errLines = append(errLines, ln)
		} else {
			other = append(other, ln)
		}
	}
	// Only engage on a real cascade: ≥ 5 error lines forming ≥ 30% of output.
	if len(errLines) < 5 || len(errLines)*100 < 30*len(lines) {
		return ""
	}
	type g struct {
		sig   string
		first string
		count int
	}
	var groups []g
	idx := map[string]int{}
	for _, ln := range errLines {
		sig := errorSig(ln)
		if i, ok := idx[sig]; ok {
			groups[i].count++
			continue
		}
		idx[sig] = len(groups)
		groups = append(groups, g{sig: sig, first: ln, count: 1})
	}
	var b strings.Builder
	if body := strings.TrimSpace(strings.Join(other, "\n")); body != "" {
		b.WriteString(body)
		b.WriteString("\n")
	}
	shown, folded := 0, 0
	for _, gr := range groups {
		if shown < maxDistinctErrors {
			b.WriteString(gr.first)
			if gr.count > 1 {
				fmt.Fprintf(&b, " … [%d similar omitted]", gr.count-1)
			}
			b.WriteString("\n")
			shown++
		} else {
			folded += gr.count
		}
	}
	if folded > 0 {
		fmt.Fprintf(&b, "… and %d more error lines in %d other signatures omitted to preserve context\n", folded, len(groups)-shown)
	}
	return b.String()
}

// compactLines collapses consecutive duplicate lines into "line" + "… [N×]",
// and drops runs of 3+ blank lines. Used to keep repetitive tool output
// (build logs, test errors) from flooding the context window.
func compactLines(s string) string {
	lines := strings.Split(s, "\n")
	var out []string
	run, prev := 1, ""
	flush := func() {
		if prev == "" {
			return
		}
		if run > 1 {
			prev = fmt.Sprintf("%s … [%d×]", prev, run)
		}
		out = append(out, prev)
	}
	blank := 0
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			blank++
			if blank > 3 {
				continue
			}
		} else {
			blank = 0
		}
		if ln == prev && prev != "" {
			run++
			continue
		}
		flush()
		prev, run = ln, 1
	}
	flush()
	joined := strings.Join(out, "\n")
	if strings.HasSuffix(s, "\n") && !strings.HasSuffix(joined, "\n") {
		joined += "\n"
	}
	return joined
}
