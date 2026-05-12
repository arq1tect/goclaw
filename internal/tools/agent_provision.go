package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/bootstrap"
	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/pkg/protocol"
)

var agentProvisionSlugRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// agentProvisionContextFileAllowlist lists files this tool will write during
// create. Mirrors the agent_context_files allowlist; intentional duplication
// to avoid cross-tool coupling.
var agentProvisionContextFileAllowlist = map[string]bool{
	bootstrap.SoulFile:           true,
	bootstrap.IdentityFile:       true,
	bootstrap.CapabilitiesFile:   true,
	bootstrap.UserPredefinedFile: true,
	bootstrap.AgentsFile:         true,
	bootstrap.UserFile:           true,
	bootstrap.BootstrapFile:      true,
	bootstrap.HeartbeatFile:      true,
}

var agentProvisionContextFileBytesMax = 100 * 1024

// AgentProvisionTool creates, deletes, and lists agents within the caller's
// tenant. It is the central admin tool for forge-style operators: a single
// atomic call creates the agent record, seeds default context files via
// bootstrap, and overlays operator-provided custom content for SOUL.md,
// IDENTITY.md, etc.
//
// Sub-actions:
//   - create: build an agent record + write context files atomically. Does
//     NOT trigger the LLM summoner — operator provides content directly.
//   - delete: hard delete (cascades through 27+ FK-linked tables). Requires
//     confirm=true to guard against accidental invocation. Operator-side
//     discipline (SOUL of the calling agent) should also surface a summary
//     to the human before calling.
//   - list: enumerate agents in the tenant with compact info.
//
// Access is governed by tools_config.allow per-agent. There is no further
// auth check inside the tool.
type AgentProvisionTool struct {
	agents           store.AgentStore
	msgBus           *bus.MessageBus
	defaultWorkspace string
}

func NewAgentProvisionTool() *AgentProvisionTool { return &AgentProvisionTool{} }

func (t *AgentProvisionTool) SetAgentStore(s store.AgentStore) { t.agents = s }

// SetMessageBus wires the broadcast bus. Optional but recommended in
// production for audit logs, cache invalidation, and TopicAgentDeleted
// orphan-cleanup downstream.
func (t *AgentProvisionTool) SetMessageBus(b *bus.MessageBus) { t.msgBus = b }

// SetDefaultWorkspace wires the default workspace root used when the
// operator does not provide an explicit workspace path on create. Final
// per-agent path is {defaultWorkspace}/{agent_key}.
func (t *AgentProvisionTool) SetDefaultWorkspace(p string) { t.defaultWorkspace = p }

func (t *AgentProvisionTool) Name() string { return "agent_provision" }

func (t *AgentProvisionTool) Description() string {
	return "Create, delete, or list agents in the tenant. The central tool for forge-style admin operators.\n\n" +
		"Actions: create (atomic create + context files), delete (hard delete with cascade, requires confirm), list (enumerate tenant agents).\n\n" +
		"create: provide agent_key (required slug), provider (required), and optionally model, display_name, thinking_level, reasoning_config, tools_config (JSON), memory_config (JSON), and the four load-bearing context files via context_files (object map: SOUL.md, IDENTITY.md, CAPABILITIES.md, USER_PREDEFINED.md, plus optional AGENTS.md/USER.md/BOOTSTRAP.md/HEARTBEAT.md). Files not provided are seeded from goclaw's default templates. Operator-provided files overwrite seed templates. The summoner is NOT triggered — context files are persisted directly.\n\n" +
		"delete: hard-deletes the agent and cascades through 27+ linked tables (sessions, memory_documents, kg_entities, agent_links, channel_instances, hooks, grants, etc.). Requires confirm=true. Cannot be undone. Before calling, surface a summary to the operator describing what will be destroyed.\n\n" +
		"list: returns compact info {agent_key, agent_uuid, display_name, provider, model, status, agent_type} for each agent in the tenant. Useful before create to avoid duplicate agent_keys."
}

func (t *AgentProvisionTool) Parameters() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"action"},
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"create", "delete", "list"},
				"description": "Operation to perform.",
			},

			// create + delete share agent_id semantics; create REQUIRES agent_key.
			"agent_id": map[string]any{
				"type":        "string",
				"description": "Required for delete: agent_key (slug) or UUID. Ignored for create (use agent_key) and list.",
			},

			// create-specific
			"agent_key": map[string]any{
				"type":        "string",
				"description": "Required for create. Slug: lowercase [a-z0-9]+hyphens, no leading/trailing hyphen. Must be unique in the tenant.",
			},
			"display_name": map[string]any{"type": "string", "description": "Human-readable name. Defaults to agent_key if omitted."},
			"provider":     map[string]any{"type": "string", "description": "Required for create. Provider name (claude-cli, openai, kimi, x, etc.)."},
			"model":        map[string]any{"type": "string", "description": "Model identifier under the provider."},
			"agent_type":   map[string]any{"type": "string", "enum": []string{"predefined"}, "description": "Only \"predefined\" is supported for create (\"open\" is deprecated). Defaults to predefined."},

			"context_window":       map[string]any{"type": "integer", "description": "LLM context window in tokens. Defaults to server config."},
			"max_tool_iterations":  map[string]any{"type": "integer", "description": "Max tool iterations per turn. Defaults to server config."},
			"max_tokens":           map[string]any{"type": "integer", "description": "Max completion tokens."},
			"thinking_level":       map[string]any{"type": "string", "description": "Legacy reasoning effort: off|low|medium|high."},
			"reasoning_config":     map[string]any{"type": "object", "description": "Reasoning policy JSON (override_mode, effort, fallback)."},
			"tools_config":         map[string]any{"type": "object", "description": "Tools policy JSON, e.g. {\"allow\":[\"tool_name\",...]}."},
			"sandbox_config":       map[string]any{"type": "object", "description": "Sandbox configuration JSON."},
			"subagents_config":     map[string]any{"type": "object", "description": "Subagent spawning configuration JSON."},
			"memory_config":        map[string]any{"type": "object", "description": "Memory subsystem config JSON. Defaults to {\"enabled\":true}."},
			"compaction_config":    map[string]any{"type": "object", "description": "Conversation compaction config JSON. Defaults to {}."},
			"context_pruning":      map[string]any{"type": "object", "description": "Context pruning config JSON."},
			"other_config":         map[string]any{"type": "object", "description": "Misc agent config bag (v3 flags, tts_params, etc.)."},
			"kg_dedup_config":      map[string]any{"type": "object", "description": "Knowledge graph dedup config JSON."},
			"workspace_sharing":    map[string]any{"type": "object", "description": "Workspace sharing config JSON."},
			"chatgpt_oauth_routing": map[string]any{"type": "object", "description": "ChatGPT OAuth pool routing config JSON."},
			"shell_deny_groups":    map[string]any{"type": "object", "description": "Shell deny-pattern groups JSON."},
			"self_evolve":          map[string]any{"type": "boolean", "description": "Allow SOUL.md self-modification."},
			"skill_evolve":         map[string]any{"type": "boolean", "description": "Allow skill_manage tool usage."},
			"skill_nudge_interval": map[string]any{"type": "integer", "description": "Skill nudge cadence in turns."},
			"is_default":           map[string]any{"type": "boolean", "description": "Mark as tenant default. Clears the flag on other agents automatically."},
			"budget_monthly_cents": map[string]any{"type": "integer", "description": "Monthly spend cap in cents. 0 = unlimited."},
			"workspace":            map[string]any{"type": "string", "description": "Filesystem workspace path. Defaults to {server_default}/{agent_key}."},
			"restrict_to_workspace": map[string]any{"type": "boolean", "description": "Confine file tools to the workspace. Defaults to true."},
			"emoji":                map[string]any{"type": "string", "description": "Dashboard badge emoji."},
			"agent_description":    map[string]any{"type": "string", "description": "Short description (stored, not used to trigger summoner)."},
			"frontmatter":          map[string]any{"type": "string", "description": "1-2 sentence expertise summary for delegation discovery (≤200 chars)."},

			"context_files": map[string]any{
				"type":        "object",
				"description": "Map of {file_name → content} for SOUL.md, IDENTITY.md, CAPABILITIES.md, USER_PREDEFINED.md (and optionally AGENTS.md, USER.md, BOOTSTRAP.md, HEARTBEAT.md). Provided files overwrite seeded templates. Each ≤100 KB.",
			},

			// delete-specific
			"confirm": map[string]any{
				"type":        "boolean",
				"description": "Required true for delete. Hard-delete cascades; cannot be undone.",
			},
		},
	}
}

func (t *AgentProvisionTool) Execute(ctx context.Context, args map[string]any) *Result {
	if t.agents == nil {
		return ErrorResult("agent_provision: agent store not wired")
	}
	action, _ := args["action"].(string)
	if action == "" {
		return ErrorResult("action parameter is required")
	}
	switch action {
	case "create":
		return t.handleCreate(ctx, args)
	case "delete":
		return t.handleDelete(ctx, args)
	case "list":
		return t.handleList(ctx, args)
	default:
		return ErrorResult(fmt.Sprintf("unknown action %q. Valid: create, delete, list", action))
	}
}

// --- create ---

func (t *AgentProvisionTool) handleCreate(ctx context.Context, args map[string]any) *Result {
	agentKey, _ := args["agent_key"].(string)
	if agentKey == "" {
		return ErrorResult("agent_key is required for create")
	}
	if !agentProvisionSlugRe.MatchString(agentKey) {
		return ErrorResult(fmt.Sprintf("agent_key %q does not match slug format ([a-z0-9] + hyphens, no leading/trailing hyphen)", agentKey))
	}

	provider, _ := args["provider"].(string)
	if provider == "" {
		return ErrorResult("provider is required for create")
	}

	// Uniqueness check before doing any work.
	if existing, _ := t.agents.GetByKey(ctx, agentKey); existing != nil {
		return ErrorResult(fmt.Sprintf("agent_key %q already exists in this tenant", agentKey))
	}

	// agent_type: predefined-only on this tool. "open" is upstream-deprecated
	// and creating one via tool is almost certainly a mistake.
	if v, ok := args["agent_type"].(string); ok && v != "" && v != store.AgentTypePredefined {
		return ErrorResult(fmt.Sprintf("agent_type %q not supported; only \"predefined\" can be created via this tool", v))
	}

	// Validate other fields (reuse agent_config validators for consistency).
	updatesForValidation := make(map[string]any, len(args))
	for k, v := range args {
		if k == "action" || k == "agent_id" || k == "agent_key" || k == "agent_type" || k == "context_files" || k == "confirm" {
			continue
		}
		if agentConfigMutableFields[k] {
			updatesForValidation[k] = v
		}
	}
	if err := validateAgentConfigUpdates(updatesForValidation); err != nil {
		return ErrorResult(err.Error())
	}

	contextFiles, err := extractContextFiles(args["context_files"])
	if err != nil {
		return ErrorResult(err.Error())
	}

	// Build the AgentData struct with sensible defaults.
	displayName, _ := args["display_name"].(string)
	if displayName == "" {
		displayName = agentKey
	}
	model, _ := args["model"].(string)
	workspaceArg, _ := args["workspace"].(string)
	workspace := workspaceArg
	if workspace == "" {
		base := t.defaultWorkspace
		if base == "" {
			// Fall back to a goclaw-conventional path. The runtime tolerates
			// late workspace resolution; this avoids forcing operators to
			// always pass workspace explicitly when the tool was not wired
			// with defaultWorkspace.
			base = "data/workspace"
		}
		workspace = fmt.Sprintf("%s/%s", base, agentKey)
	}

	ag := &store.AgentData{
		AgentKey:            agentKey,
		DisplayName:         displayName,
		Provider:            provider,
		Model:               model,
		AgentType:           store.AgentTypePredefined,
		Status:              store.AgentStatusActive,
		Workspace:           workspace,
		RestrictToWorkspace: true,
		TenantID:            store.TenantIDFromContext(ctx),
		OwnerID:             store.UserIDFromContext(ctx),
	}

	applyOptionalScalarsOnCreate(ag, args)
	if err := applyOptionalJSONOnCreate(ag, args); err != nil {
		return ErrorResult(err.Error())
	}

	// Defaults for JSONB fields that handleCreate sets when caller doesn't.
	if len(ag.MemoryConfig) == 0 {
		ag.MemoryConfig = json.RawMessage(`{"enabled":true}`)
	}
	if len(ag.CompactionConfig) == 0 {
		ag.CompactionConfig = json.RawMessage(`{}`)
	}

	// 1. Create record.
	if err := t.agents.Create(ctx, ag); err != nil {
		if strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "23505") {
			return ErrorResult(fmt.Sprintf("agent_key %q already exists in this tenant", agentKey))
		}
		return ErrorResult(fmt.Sprintf("create failed: %v", err))
	}

	// 2. Seed default templates for any standard files we did NOT provide.
	//    SeedToStore is idempotent — only writes files that have no content yet.
	if _, err := bootstrap.SeedToStore(ctx, t.agents, ag.ID, ag.AgentType); err != nil {
		slog.Warn("agent_provision.create: SeedToStore failed (non-fatal)", "agent", ag.AgentKey, "error", err)
	}

	// 3. Overlay operator-provided custom content.
	var writeErrs []string
	for name, content := range contextFiles {
		if err := t.agents.SetAgentContextFile(ctx, ag.ID, name, content); err != nil {
			writeErrs = append(writeErrs, fmt.Sprintf("%s: %v", name, err))
			slog.Warn("agent_provision.create: failed to write context file", "agent", ag.AgentKey, "file", name, "error", err)
		}
	}

	// 4. Emit audit.
	t.emitAudit(ctx, "agent.created", ag.ID.String())

	// Build response.
	resp := map[string]any{
		"agent_id":     ag.AgentKey,
		"agent_uuid":   ag.ID.String(),
		"display_name": ag.DisplayName,
		"provider":     ag.Provider,
		"model":        ag.Model,
		"agent_type":   ag.AgentType,
		"status":       ag.Status,
		"workspace":    ag.Workspace,
		"context_files_written": sortedKeys(contextFiles),
	}
	if len(writeErrs) > 0 {
		resp["context_file_errors"] = writeErrs
	}
	body, _ := json.MarshalIndent(resp, "", "  ")
	return NewResult(string(body))
}

// --- delete ---

func (t *AgentProvisionTool) handleDelete(ctx context.Context, args map[string]any) *Result {
	agentID, _ := args["agent_id"].(string)
	if agentID == "" {
		return ErrorResult("agent_id is required for delete")
	}
	confirm, _ := args["confirm"].(bool)
	if !confirm {
		return ErrorResult("delete requires confirm=true. This action is irreversible: it cascades through 27+ tables (sessions, memory_documents, kg_entities, agent_links, channel_instances, hooks, grants, context_files). Surface a summary to the operator and re-call with confirm=true.")
	}

	ag, err := t.resolveAgent(ctx, agentID)
	if err != nil {
		return ErrorResult(err.Error())
	}

	if err := t.agents.Delete(ctx, ag.ID); err != nil {
		return ErrorResult(fmt.Sprintf("delete failed: %v", err))
	}

	// Cache invalidate (matches HTTP handleDelete).
	t.emitCacheInvalidate(bus.CacheKindAgent, ag.AgentKey)

	// TopicAgentDeleted broadcast (matches WS handler; HTTP handler currently
	// omits this, but our tool emits it so downstream orphan cleanup —
	// provider deregistration etc. — fires consistently).
	if t.msgBus != nil {
		t.msgBus.Broadcast(bus.Event{
			Name: bus.TopicAgentDeleted,
			Payload: bus.AgentDeletedPayload{
				AgentKey: ag.AgentKey,
				Provider: ag.Provider,
				TenantID: store.TenantIDFromContext(ctx),
			},
		})
	}

	t.emitAudit(ctx, "agent.deleted", ag.ID.String())

	resp := map[string]any{
		"agent_id":   ag.AgentKey,
		"agent_uuid": ag.ID.String(),
		"deleted":    true,
		"note":       "Hard delete cascaded through FK-linked tables. Cannot be undone.",
	}
	body, _ := json.MarshalIndent(resp, "", "  ")
	return NewResult(string(body))
}

// --- list ---

func (t *AgentProvisionTool) handleList(ctx context.Context, _ map[string]any) *Result {
	rows, err := t.agents.List(ctx, "")
	if err != nil {
		return ErrorResult(fmt.Sprintf("list failed: %v", err))
	}
	out := make([]map[string]any, 0, len(rows))
	for _, ag := range rows {
		out = append(out, map[string]any{
			"agent_id":     ag.AgentKey,
			"agent_uuid":   ag.ID.String(),
			"display_name": ag.DisplayName,
			"provider":     ag.Provider,
			"model":        ag.Model,
			"status":       ag.Status,
			"agent_type":   ag.AgentType,
		})
	}
	resp := map[string]any{
		"count":  len(out),
		"agents": out,
	}
	body, _ := json.MarshalIndent(resp, "", "  ")
	return NewResult(string(body))
}

// --- helpers ---

func (t *AgentProvisionTool) resolveAgent(ctx context.Context, agentID string) (*store.AgentData, error) {
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

func (t *AgentProvisionTool) emitCacheInvalidate(kind, key string) {
	if t.msgBus == nil {
		return
	}
	t.msgBus.Broadcast(bus.Event{
		Name:    protocol.EventCacheInvalidate,
		Payload: bus.CacheInvalidatePayload{Kind: kind, Key: key},
	})
}

// emitAudit broadcasts an audit event with actor identification taken from
// context. Mirrors http.emitAudit but ctx-driven (no *http.Request).
func (t *AgentProvisionTool) emitAudit(ctx context.Context, action, entityID string) {
	if t.msgBus == nil {
		return
	}
	actorID := store.UserIDFromContext(ctx)
	if actorID == "" {
		actorID = "system"
	}
	t.msgBus.Broadcast(bus.Event{
		Name: protocol.EventAuditLog,
		Payload: bus.AuditEventPayload{
			ActorType:  "agent",
			ActorID:    actorID,
			Action:     action,
			EntityType: "agent",
			EntityID:   entityID,
			TenantID:   store.TenantIDFromContext(ctx),
		},
	})
}

// applyOptionalScalarsOnCreate copies optional scalar fields from args
// into the AgentData struct. Each field is only written if present and
// of the right Go type — bad types are silently ignored (validation has
// already run, so this is defensive).
func applyOptionalScalarsOnCreate(ag *store.AgentData, args map[string]any) {
	if v, ok := args["context_window"]; ok {
		if n, ok := asInt(v); ok {
			ag.ContextWindow = n
		}
	}
	if v, ok := args["max_tool_iterations"]; ok {
		if n, ok := asInt(v); ok {
			ag.MaxToolIterations = n
		}
	}
	if v, ok := args["max_tokens"]; ok {
		if n, ok := asInt(v); ok {
			ag.MaxTokens = n
		}
	}
	if v, ok := args["thinking_level"].(string); ok {
		ag.ThinkingLevel = v
	}
	if v, ok := args["emoji"].(string); ok {
		ag.Emoji = v
	}
	if v, ok := args["agent_description"].(string); ok {
		ag.AgentDescription = v
	}
	if v, ok := args["frontmatter"].(string); ok {
		ag.Frontmatter = v
	}
	if v, ok := args["self_evolve"].(bool); ok {
		ag.SelfEvolve = v
	}
	if v, ok := args["skill_evolve"].(bool); ok {
		ag.SkillEvolve = v
	}
	if v, ok := args["skill_nudge_interval"]; ok {
		if n, ok := asInt(v); ok {
			ag.SkillNudgeInterval = n
		}
	}
	if v, ok := args["is_default"].(bool); ok {
		ag.IsDefault = v
	}
	if v, ok := args["budget_monthly_cents"]; ok {
		if n, ok := asInt(v); ok {
			ag.BudgetMonthlyCents = &n
		}
	}
	if v, ok := args["restrict_to_workspace"].(bool); ok {
		ag.RestrictToWorkspace = v
	}
}

// applyOptionalJSONOnCreate copies JSONB fields from args into AgentData.
// Returns an error if any field is unmarshalable.
func applyOptionalJSONOnCreate(ag *store.AgentData, args map[string]any) error {
	type jsonField struct {
		key string
		dst *json.RawMessage
	}
	fields := []jsonField{
		{"tools_config", &ag.ToolsConfig},
		{"sandbox_config", &ag.SandboxConfig},
		{"subagents_config", &ag.SubagentsConfig},
		{"memory_config", &ag.MemoryConfig},
		{"compaction_config", &ag.CompactionConfig},
		{"context_pruning", &ag.ContextPruning},
		{"other_config", &ag.OtherConfig},
		{"reasoning_config", &ag.ReasoningConfig},
		{"workspace_sharing", &ag.WorkspaceSharing},
		{"chatgpt_oauth_routing", &ag.ChatGPTOAuthRouting},
		{"shell_deny_groups", &ag.ShellDenyGroups},
		{"kg_dedup_config", &ag.KGDedupConfig},
	}
	for _, f := range fields {
		v, ok := args[f.key]
		if !ok || v == nil {
			continue
		}
		raw, err := marshalAnyAsJSON(v)
		if err != nil {
			return fmt.Errorf("%s: %v", f.key, err)
		}
		*f.dst = raw
	}
	return nil
}

// marshalAnyAsJSON converts a Go value into json.RawMessage. Accepts
// map/slice (re-marshaled) or string (parsed-then-remarshaled to confirm
// validity).
func marshalAnyAsJSON(v any) (json.RawMessage, error) {
	switch x := v.(type) {
	case map[string]any, []any:
		return json.Marshal(x)
	case string:
		if x == "" {
			return nil, nil
		}
		var parsed any
		if err := json.Unmarshal([]byte(x), &parsed); err != nil {
			return nil, fmt.Errorf("invalid JSON: %v", err)
		}
		return json.Marshal(parsed)
	default:
		return nil, fmt.Errorf("expected object/array/JSON-string, got %T", v)
	}
}

// extractContextFiles pulls the context_files map from args, validating
// that keys are in the allowlist and content is within size limits.
func extractContextFiles(raw any) (map[string]string, error) {
	if raw == nil {
		return nil, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("context_files must be an object")
	}
	out := make(map[string]string, len(m))
	for name, content := range m {
		if !agentProvisionContextFileAllowlist[name] {
			return nil, fmt.Errorf("context_files: %q is not an allowed context file (allowed: %s)", name, sortedKeysStr(agentProvisionContextFileAllowlist))
		}
		s, ok := content.(string)
		if !ok {
			return nil, fmt.Errorf("context_files[%q]: content must be a string, got %T", name, content)
		}
		if len(s) > agentProvisionContextFileBytesMax {
			return nil, fmt.Errorf("context_files[%q]: content exceeds %d byte limit (got %d bytes)", name, agentProvisionContextFileBytesMax, len(s))
		}
		out[name] = s
	}
	return out, nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Cheap insertion-style sort; the slice is tiny (<10).
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

func sortedKeysStr(m map[string]bool) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	return strings.Join(keys, ", ")
}
