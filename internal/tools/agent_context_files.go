package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/bootstrap"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// Size and response caps for agent_context_files tool.
const (
	// Maximum bytes accepted on write. UPSERT will reject larger payloads.
	agentContextFilesMaxContentBytes = 100 * 1024

	// Soft cap on content returned in read / post-write response. Larger
	// content is truncated to head + tail with a marker between, and the
	// caller is told to use action=read for the full file.
	agentContextFilesResponseCapBytes  = 20 * 1024
	agentContextFilesResponseHeadBytes = 15 * 1024
	agentContextFilesResponseTailBytes = 5 * 1024
)

// allowedContextFiles enumerates the files this tool may operate on. Mirrors
// (but does not import) the allowlist used by gateway/methods/agents_files.go.
// Keeping a local copy avoids touching upstream files for merge-conflict
// reasons; if a new bootstrap file is added that we want to expose, add it
// here explicitly.
var allowedContextFiles = []string{
	bootstrap.SoulFile,
	bootstrap.IdentityFile,
	bootstrap.CapabilitiesFile,
	bootstrap.UserPredefinedFile,
	bootstrap.AgentsFile,
	bootstrap.UserFile,
	bootstrap.BootstrapFile,
	bootstrap.HeartbeatFile,
	bootstrap.MemoryJSONFile,
}

// protectedFromDeletion blocks delete for files whose absence breaks the
// running agent. Operator may still delete these out-of-band through direct
// DB access if they really mean it; the tool refuses by design.
var protectedFromDeletion = map[string]bool{
	bootstrap.SoulFile:           true,
	bootstrap.IdentityFile:       true,
	bootstrap.CapabilitiesFile:   true,
	bootstrap.UserPredefinedFile: true,
	bootstrap.AgentsFile:         true,
	bootstrap.HeartbeatFile:      true,
}

// AgentContextFilesTool provides cross-agent CRUD for agent-level context
// files (SOUL.md, IDENTITY.md, CAPABILITIES.md, USER_PREDEFINED.md, etc.).
//
// Intended for an admin-class agent (e.g. forge) that provisions and edits
// other agents on this goclaw instance. Access is governed by
// tools_config.allow per-agent — only agents that include
// "agent_context_files" in their allow list can call this tool. There is no
// further role/auth check inside the tool: presence in allow == authorization.
//
// All operations are tenant-scoped via the agent store's GetByKey/GetByID —
// agents in other tenants cannot be touched.
//
// Write returns the post-write state (re-read from DB) for verification. This
// matches the REST PUT-returns-resource convention and removes a class of bugs
// where the caller forgets to verify after mutate.
type AgentContextFilesTool struct {
	agents store.AgentStore
}

func NewAgentContextFilesTool() *AgentContextFilesTool {
	return &AgentContextFilesTool{}
}

// SetAgentStore wires the dependency. Called during gateway setup.
func (t *AgentContextFilesTool) SetAgentStore(s store.AgentStore) {
	t.agents = s
}

func (t *AgentContextFilesTool) Name() string { return "agent_context_files" }

func (t *AgentContextFilesTool) Description() string {
	return "Cross-agent CRUD for agent-level context files (SOUL.md, IDENTITY.md, CAPABILITIES.md, USER_PREDEFINED.md, AGENTS.md, USER.md, BOOTSTRAP.md, HEARTBEAT.md, MEMORY.json).\n\n" +
		"Actions: list, read, write, delete.\n\n" +
		"Use this to read existing files before editing (pre-read pattern), to write a new version after operator approval, or to delete optional files (BOOTSTRAP, USER, MEMORY.json). " +
		"Files that load-bear the agent (SOUL, IDENTITY, CAPABILITIES, USER_PREDEFINED, AGENTS, HEARTBEAT) cannot be deleted through this tool.\n\n" +
		"Write returns the post-write state (re-read from DB) for verification. Response content is capped at 20 KB " +
		"(head 15 KB + tail 5 KB with a truncation marker between) — call `read` for full content of larger files.\n\n" +
		"All operations are tenant-scoped: only agents within the same tenant as the caller are accessible."
}

func (t *AgentContextFilesTool) Parameters() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"action"},
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"list", "read", "write", "delete"},
				"description": "Operation to perform.",
			},
			"agent_id": map[string]any{
				"type":        "string",
				"description": "Target agent — either agent_key (slug, e.g. 'forge') or UUID. Required for all actions.",
			},
			"file_name": map[string]any{
				"type":        "string",
				"enum":        allowedContextFiles,
				"description": "Required for read, write, delete. Must be one of the allowlisted files.",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "Required for write. Full replacement content (not a patch). Empty string is allowed.",
			},
		},
	}
}

func (t *AgentContextFilesTool) Execute(ctx context.Context, args map[string]any) *Result {
	if t.agents == nil {
		return ErrorResult("agent_context_files: agent store not wired")
	}

	action, _ := args["action"].(string)
	if action == "" {
		return ErrorResult("action parameter is required")
	}

	switch action {
	case "list":
		return t.handleList(ctx, args)
	case "read":
		return t.handleRead(ctx, args)
	case "write":
		return t.handleWrite(ctx, args)
	case "delete":
		return t.handleDelete(ctx, args)
	default:
		return ErrorResult(fmt.Sprintf("unknown action %q. Valid: list, read, write, delete", action))
	}
}

// resolveAgent translates agent_id (key or UUID string) into the AgentData.
// Tenant scoping is enforced by the store layer — agents in other tenants
// resolve as "not found".
func (t *AgentContextFilesTool) resolveAgent(ctx context.Context, agentID string) (*store.AgentData, error) {
	if agentID == "" {
		return nil, fmt.Errorf("agent_id is required")
	}
	if id, err := uuid.Parse(agentID); err == nil {
		ag, err := t.agents.GetByID(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("agent not found: %s", agentID)
		}
		return ag, nil
	}
	ag, err := t.agents.GetByKey(ctx, agentID)
	if err != nil {
		return nil, fmt.Errorf("agent not found: %s", agentID)
	}
	return ag, nil
}

func validateFileName(name string) error {
	if name == "" {
		return fmt.Errorf("file_name is required")
	}
	for _, allowed := range allowedContextFiles {
		if name == allowed {
			return nil
		}
	}
	return fmt.Errorf("file_name %q not in allowlist; allowed: %s", name, strings.Join(allowedContextFiles, ", "))
}

// handleList returns the list of allowlisted files for the target agent with
// existence flag and size. Files not present in the DB are reported with
// exists=false and size=0 so the caller sees the full possible surface.
func (t *AgentContextFilesTool) handleList(ctx context.Context, args map[string]any) *Result {
	agentID, _ := args["agent_id"].(string)
	ag, err := t.resolveAgent(ctx, agentID)
	if err != nil {
		return ErrorResult(err.Error())
	}

	rows, err := t.agents.GetAgentContextFiles(ctx, ag.ID)
	if err != nil {
		return ErrorResult(fmt.Sprintf("failed to list context files: %v", err))
	}

	present := make(map[string]int, len(rows))
	for _, r := range rows {
		present[r.FileName] = len(r.Content)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "agent: %s (%s)\n", ag.AgentKey, ag.ID)
	sb.WriteString("files:\n")
	for _, name := range allowedContextFiles {
		if size, ok := present[name]; ok {
			fmt.Fprintf(&sb, "  - %-20s present  size=%d bytes\n", name, size)
		} else {
			fmt.Fprintf(&sb, "  - %-20s missing\n", name)
		}
	}
	return NewResult(sb.String())
}

// handleRead returns the content of a single file, or an error if missing.
func (t *AgentContextFilesTool) handleRead(ctx context.Context, args map[string]any) *Result {
	agentID, _ := args["agent_id"].(string)
	fileName, _ := args["file_name"].(string)

	if err := validateFileName(fileName); err != nil {
		return ErrorResult(err.Error())
	}

	ag, err := t.resolveAgent(ctx, agentID)
	if err != nil {
		return ErrorResult(err.Error())
	}

	rows, err := t.agents.GetAgentContextFiles(ctx, ag.ID)
	if err != nil {
		return ErrorResult(fmt.Sprintf("failed to read context files: %v", err))
	}

	for _, r := range rows {
		if r.FileName == fileName {
			return NewResult(formatFileResponse(ag.AgentKey, ag.ID, fileName, r.Content, "read"))
		}
	}
	return ErrorResult(fmt.Sprintf("file %q not found for agent %q", fileName, ag.AgentKey))
}

// handleWrite UPSERTs the file then re-reads it from the DB and returns the
// post-write state. The re-read is real verification: it catches the
// theoretical case where another concurrent writer overwrote between our
// write and the response.
func (t *AgentContextFilesTool) handleWrite(ctx context.Context, args map[string]any) *Result {
	agentID, _ := args["agent_id"].(string)
	fileName, _ := args["file_name"].(string)
	content, hasContent := args["content"].(string)

	if err := validateFileName(fileName); err != nil {
		return ErrorResult(err.Error())
	}
	if !hasContent {
		return ErrorResult("write requires content field (empty string allowed)")
	}
	if len(content) > agentContextFilesMaxContentBytes {
		return ErrorResult(fmt.Sprintf("content exceeds %d byte limit (got %d bytes)", agentContextFilesMaxContentBytes, len(content)))
	}

	ag, err := t.resolveAgent(ctx, agentID)
	if err != nil {
		return ErrorResult(err.Error())
	}

	// Determine whether file existed before, for the action_taken signal.
	priorRows, err := t.agents.GetAgentContextFiles(ctx, ag.ID)
	if err != nil {
		return ErrorResult(fmt.Sprintf("failed to read prior state: %v", err))
	}
	prevSize := -1
	for _, r := range priorRows {
		if r.FileName == fileName {
			prevSize = len(r.Content)
			break
		}
	}

	if err := t.agents.SetAgentContextFile(ctx, ag.ID, fileName, content); err != nil {
		return ErrorResult(fmt.Sprintf("write failed: %v", err))
	}

	// Re-read from DB for true post-state verification.
	rows, err := t.agents.GetAgentContextFiles(ctx, ag.ID)
	if err != nil {
		return ErrorResult(fmt.Sprintf("write succeeded but re-read failed: %v", err))
	}
	var stored string
	found := false
	for _, r := range rows {
		if r.FileName == fileName {
			stored = r.Content
			found = true
			break
		}
	}
	if !found {
		return ErrorResult("write reported success but file is missing on re-read")
	}

	action := "updated"
	if prevSize < 0 {
		action = "created"
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "write %s: agent=%s file=%s size=%d", action, ag.AgentKey, fileName, len(stored))
	if prevSize >= 0 {
		fmt.Fprintf(&sb, " previous_size=%d", prevSize)
	}
	sb.WriteString("\n\n")
	sb.WriteString("--- stored content ---\n")
	sb.WriteString(capForResponse(stored))

	return NewResult(sb.String())
}

// handleDelete refuses protected files and is idempotent for missing files.
func (t *AgentContextFilesTool) handleDelete(ctx context.Context, args map[string]any) *Result {
	agentID, _ := args["agent_id"].(string)
	fileName, _ := args["file_name"].(string)

	if err := validateFileName(fileName); err != nil {
		return ErrorResult(err.Error())
	}
	if protectedFromDeletion[fileName] {
		return ErrorResult(fmt.Sprintf("file %q is protected from deletion (load-bearing for the agent). Protected files: SOUL.md, IDENTITY.md, CAPABILITIES.md, USER_PREDEFINED.md, AGENTS.md, HEARTBEAT.md", fileName))
	}

	ag, err := t.resolveAgent(ctx, agentID)
	if err != nil {
		return ErrorResult(err.Error())
	}

	// Look up prior size for the response, then delete. The store delete
	// method is idempotent: returns (false, nil) if the row was already
	// absent.
	priorRows, err := t.agents.GetAgentContextFiles(ctx, ag.ID)
	if err != nil {
		return ErrorResult(fmt.Sprintf("failed to read prior state: %v", err))
	}
	prevSize := 0
	existed := false
	for _, r := range priorRows {
		if r.FileName == fileName {
			prevSize = len(r.Content)
			existed = true
			break
		}
	}

	if !existed {
		return NewResult(fmt.Sprintf("delete noop: agent=%s file=%s (was not present)", ag.AgentKey, fileName))
	}

	deleted, err := t.agents.DeleteAgentContextFile(ctx, ag.ID, fileName)
	if err != nil {
		return ErrorResult(fmt.Sprintf("delete failed: %v", err))
	}
	if !deleted {
		// Race: someone else deleted between our list and our delete.
		return NewResult(fmt.Sprintf("delete noop: agent=%s file=%s (raced with another deletion)", ag.AgentKey, fileName))
	}
	return NewResult(fmt.Sprintf("delete ok: agent=%s file=%s previous_size=%d", ag.AgentKey, fileName, prevSize))
}

// formatFileResponse renders a metadata header + capped content body.
func formatFileResponse(agentKey string, agentID uuid.UUID, fileName, content, action string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s: agent=%s (%s) file=%s size=%d\n\n", action, agentKey, agentID, fileName, len(content))
	sb.WriteString("--- content ---\n")
	sb.WriteString(capForResponse(content))
	return sb.String()
}

// capForResponse returns content unchanged if under the response cap, or
// head + truncation marker + tail otherwise. The marker tells the LLM how
// many bytes are hidden and how to fetch the full content.
func capForResponse(content string) string {
	if len(content) <= agentContextFilesResponseCapBytes {
		return content
	}
	head := content[:agentContextFilesResponseHeadBytes]
	tail := content[len(content)-agentContextFilesResponseTailBytes:]
	hidden := len(content) - agentContextFilesResponseHeadBytes - agentContextFilesResponseTailBytes
	return fmt.Sprintf("%s\n\n[... truncated %d bytes of total %d; call action=read for full content ...]\n\n%s",
		head, hidden, len(content), tail)
}
