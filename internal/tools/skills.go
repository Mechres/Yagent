package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Mechres/Yagent/internal/llm"
	"github.com/Mechres/Yagent/internal/skills"
)

// ---------- skills_list ----------

type skillsListTool struct{ store *skills.Store }

var skillsListSchema = fnSchema("skills_list", "list all saved skills (procedural memory): name, category, source, description; always called before creating a skill to avoid duplicates",
	map[string]any{}, []string{})

func (t *skillsListTool) Schema() llm.ToolSchema { return skillsListSchema }
func (t *skillsListTool) Risk() RiskLevel        { return RiskReadOnly }

func (t *skillsListTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	metas := t.store.List()
	if len(metas) == 0 {
		return "no skills saved yet", nil
	}
	var b strings.Builder
	for _, m := range metas {
		root := m.Root
		fmt.Fprintf(&b, "- %s [%s, %s", m.Name, m.Category, m.Source)
		if root == skills.RootProject {
			b.WriteString(", project")
		}
		fmt.Fprintf(&b, "]: %s\n", m.Description)
	}
	return capResult(b.String(), maxResultBytes), nil
}

// ---------- skill_view ----------

type skillViewTool struct{ store *skills.Store }

var skillViewSchema = fnSchema("skill_view", "load a skill's full SKILL.md (or a reference file by path); use when a listed skill's trigger matches the task",
	map[string]any{
		"name": strProp("skill name"),
		"path": strProp("reference file under the skill, e.g. references/checklist.md (optional)"),
	}, []string{"name"})

func (t *skillViewTool) Schema() llm.ToolSchema { return skillViewSchema }
func (t *skillViewTool) Risk() RiskLevel        { return RiskReadOnly }

func (t *skillViewTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Name string `json:"name"`
		Path string `json:"path,omitempty"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	if a.Name == "" {
		return "", validationErrorf(`argument "name" is required`)
	}
	content, warning, err := t.store.View(a.Name, a.Path)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	if warning != "" {
		return "\n" + warning + "\n\n" + content, nil
	}
	return content, nil
}

// ---------- skill_manage ----------

type skillManageTool struct {
	store         *skills.Store
	writeApproval bool
}

type skillManageArgs struct {
	Action      string `json:"action"`
	Name        string `json:"name"`
	Content     string `json:"content,omitempty"`
	Category    string `json:"category,omitempty"`
	OldString   string `json:"old_string,omitempty"`
	NewString   string `json:"new_string,omitempty"`
	FilePath    string `json:"file_path,omitempty"`
	FileContent string `json:"file_content,omitempty"`
	Scope       string `json:"scope,omitempty"`
}

var skillManageSchema = fnSchema("skill_manage", "write to a skill (procedural memory): create/patch/edit/delete/write_file/remove_file/pin/unpin/archive/restore; writes are gated — with the approval gate on they are staged for review, not applied",
	map[string]any{
		"action":       strProp("create, patch, edit, delete, write_file, remove_file, pin, unpin, archive, restore"),
		"name":         strProp("skill slug [a-z][a-z0-9_-]*"),
		"content":      strProp("full SKILL.md (create/edit): frontmatter + When to Use + Procedure + Pitfalls + Verification"),
		"category":     strProp("category dir for create, optional"),
		"old_string":   strProp("exact text to replace (patch); must match exactly once"),
		"new_string":   strProp("replacement text (patch)"),
		"file_path":    strProp("reference file path relative to the skill dir (write_file/remove_file)"),
		"file_content": strProp("reference file content (write_file)"),
		"scope":        strProp("global (default) or project"),
	},
	[]string{"action", "name"})

// SelfGated means skill_manage handles its own approval (the skills gate),
// so the agent's generic y/n approver must not fire for it.
func (t *skillManageTool) SelfGated() bool { return true }

func (t *skillManageTool) Schema() llm.ToolSchema { return skillManageSchema }
func (t *skillManageTool) Risk() RiskLevel        { return RiskWrite }

func (t *skillManageTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a skillManageArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	if a.Action == "" {
		return "", validationErrorf(`argument "action" is required: create|patch|edit|delete|write_file|remove_file|pin|unpin|archive|restore`)
	}
	switch a.Action {
	case skills.ActionPin, skills.ActionUnpin:
		if a.Name == "" {
			return "", validationErrorf(`argument "name" is required`)
		}
		if err := t.store.SetPinned(a.Name, a.Action == skills.ActionPin); err != nil {
			return fmt.Sprintf("error: %v", err), nil
		}
		return a.Action + "ned skill", nil
	case skills.ActionArchive:
		if err := t.store.Archive(a.Name); err != nil {
			return fmt.Sprintf("error: %v", err), nil
		}
		return "archived skill (recoverable)", nil
	case skills.ActionRestore:
		if err := t.store.Restore(a.Name); err != nil {
			return fmt.Sprintf("error: %v", err), nil
		}
		return "restored skill", nil
	}
	if a.Name == "" {
		return "", validationErrorf(`argument "name" is required`)
	}
	if t.store == nil {
		return "error: skills store is not configured for this session", nil
	}
	op := skills.Op{
		Action:      a.Action,
		Name:        a.Name,
		Scope:       a.Scope,
		Content:     a.Content,
		Category:    a.Category,
		OldString:   a.OldString,
		NewString:   a.NewString,
		FilePath:    a.FilePath,
		FileContent: a.FileContent,
	}

	if t.writeApproval {
		if t.store.StagedCount() >= skills.MaxStagedPerSession {
			return "", validationErrorf("this session already staged %d skill writes (cap %d); stop proposing new skills and instead patch existing ones", t.store.StagedCount(), skills.MaxStagedPerSession)
		}
		id, warning, err := t.store.Stage(op)
		if err != nil {
			return "", err
		}
		msg := fmt.Sprintf("staged %s of skill %q (id %s) for review; it is NOT applied yet", a.Action, a.Name, id)
		if warning != "" {
			msg += "\n" + warning
		}
		return msg, nil
	}

	warning, err := t.store.Apply(op)
	if err != nil {
		return "", err
	}
	msg := fmt.Sprintf("%s skill %q applied", a.Action, a.Name)
	if warning != "" {
		msg += "\n" + warning
	}
	return msg, nil
}
