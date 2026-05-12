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

// agentConfigSlugRe mirrors internal/http/validate.go slugRe. Duplicated to
// avoid tools → http package dependency.
var agentConfigSlugRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// agentConfigMutableFields is the allowlist of fields that this tool may
// update. Mirrors http.agentAllowedFields with agent_type EXCLUDED (treated
// as immutable here — converting an existing agent between open/predefined
// is a breaking change for any running sessions and is out of scope).
//
// agent_key IS included; renames are supported, with router-cache
// invalidation handled by the emitted cache event + store.Update.
var agentConfigMutableFields = map[string]bool{
	"agent_key": true, "display_name": true,
	"provider": true, "model": true, "status": true,
	"context_window": true, "max_tool_iterations": true,
	"workspace":          true,
	"frontmatter":        true,
	"compaction_config":  true,
	"memory_config":      true,
	"other_config":       true,
	"tools_config":       true,
	"sandbox_config":     true,
	"context_pruning":    true,
	"is_default":         true,
	"budget_monthly_cents": true,
	"subagents_config":   true,
	"emoji":              true,
	"agent_description":  true,
	"thinking_level":     true,
	"max_tokens":         true,
	"self_evolve":        true,
	"skill_evolve":       true,
	"skill_nudge_interval": true,
	"reasoning_config":   true,
	"workspace_sharing":  true,
	"chatgpt_oauth_routing": true,
	"shell_deny_groups":  true,
	"kg_dedup_config":    true,
}

var agentConfigThinkingLevels = map[string]bool{
	"off": true, "low": true, "medium": true, "high": true,
}

var agentConfigReasoningEfforts = map[string]bool{
	"off": true, "auto": true, "none": true, "minimal": true,
	"low": true, "medium": true, "high": true, "xhigh": true,
}

// agentConfigAllowedStatuses lists statuses the operator may set via this
// tool. "summoning" is intentionally excluded — it is an internal async-flag
// owned by the summoner subsystem and setting it manually leaves the agent
// in a stuck state.
var agentConfigAllowedStatuses = map[string]bool{
	store.AgentStatusActive:       true,
	store.AgentStatusSummonFailed: true,
}

// agentConfigJSONFields lists fields that must parse as JSON before being
// stored. Source of truth for "is this a JSONB field" within the tool.
var agentConfigJSONFields = map[string]bool{
	"tools_config":          true,
	"sandbox_config":        true,
	"subagents_config":      true,
	"memory_config":         true,
	"compaction_config":     true,
	"context_pruning":       true,
	"other_config":          true,
	"reasoning_config":      true,
	"workspace_sharing":     true,
	"chatgpt_oauth_routing": true,
	"shell_deny_groups":     true,
	"kg_dedup_config":       true,
}

// AgentConfigTool reads and patch-updates the configuration of any agent in
// the caller's tenant. It does not create or delete agents (see
// agent_provision) and does not modify context files (see agent_context_files).
//
// Access is governed by tools_config.allow per-agent — only agents that
// include "agent_config" in their allow list can call this tool.
//
// Updates are partial-patch: only fields present in the args are changed.
// JSONB fields are full-replace (no deep merge); for tools_config and
// similar, the standard workflow is read → modify → write.
//
// Side effects replicated from the HTTP handler (handleUpdate):
//   - router + bootstrap cache invalidation via msgBus
//   - IDENTITY.md Name sync when display_name changes
//   - EventAgentStatusChanged broadcast on status change
//   - tenant-scoped audit event
//
// If msgBus is not wired (e.g. in tests), cache invalidation and broadcasts
// are silently skipped; the update itself still persists. In production
// gateway wiring msgBus is always present.
type AgentConfigTool struct {
	agents store.AgentStore
	msgBus *bus.MessageBus
}

func NewAgentConfigTool() *AgentConfigTool { return &AgentConfigTool{} }

func (t *AgentConfigTool) SetAgentStore(s store.AgentStore) { t.agents = s }

// SetMessageBus wires the broadcast bus for cache invalidation + status
// change events. Optional (tool degrades gracefully without it).
func (t *AgentConfigTool) SetMessageBus(b *bus.MessageBus) { t.msgBus = b }

func (t *AgentConfigTool) Name() string { return "agent_config" }

func (t *AgentConfigTool) Description() string {
	return "Read or patch-update configuration of any agent in the tenant. Does NOT create, delete, or modify context files.\n\n" +
		"Actions: read (returns the current config), update (partial-patch: only fields present in args change).\n\n" +
		"Common updateable fields: display_name, provider, model, thinking_level, reasoning_config (JSON), tools_config (JSON), memory_config (JSON), sandbox_config (JSON), self_evolve, skill_evolve, context_window, max_tool_iterations, max_tokens, status, agent_description, emoji, frontmatter. " +
		"For full field list see Parameters.\n\n" +
		"JSONB fields (tools_config, reasoning_config, etc.) are full-replace, not deep-merged. To change one key in a JSON field, read the agent first, modify locally, then write back the full object.\n\n" +
		"Note on reasoning: thinking_level (legacy 4-tier off/low/medium/high) and reasoning_config (newer 8-tier effort) coexist. Set both for consistency if changing either — the runtime uses reasoning_config when present and falls back to thinking_level.\n\n" +
		"Status: only \"active\" and \"summon_failed\" are settable; \"summoning\" is owned by the async summoner subsystem and cannot be set directly.\n\n" +
		"On update, the tool returns the diff (fields_changed + before + after) for verification. Call read separately to see the full post-state."
}

func (t *AgentConfigTool) Parameters() map[string]any {
	props := map[string]any{
		"action": map[string]any{
			"type":        "string",
			"enum":        []string{"read", "update"},
			"description": "Operation to perform.",
		},
		"agent_id": map[string]any{
			"type":        "string",
			"description": "Target agent — agent_key (slug, e.g. 'forge') or UUID. Required.",
		},
	}

	// Add each mutable field as an optional top-level param. Types are
	// loose ("string"/"integer"/"boolean"/"object") so the LLM can supply
	// JSON literals for JSONB fields without escaping.
	type fieldSpec struct {
		name string
		typ  string
		desc string
	}
	specs := []fieldSpec{
		{"agent_key", "string", "Rename the agent slug. Lowercase alphanumeric + hyphens; cannot start/end with hyphen. Router cache is invalidated on rename."},
		{"display_name", "string", "Human-readable name. Also syncs into the agent's IDENTITY.md Name field."},
		{"provider", "string", "Provider name (e.g. claude-cli, openai, kimi)."},
		{"model", "string", "Model identifier under the provider (e.g. opus, gpt-5, kimi-for-coding)."},
		{"status", "string", "Lifecycle status. Settable values: \"active\", \"summon_failed\". \"summoning\" is reserved."},
		{"context_window", "integer", "LLM context window in tokens. Must be > 0."},
		{"max_tool_iterations", "integer", "Maximum tool iterations per turn. Must be > 0."},
		{"max_tokens", "integer", "Max completion tokens. > 0 to set, omit to leave unchanged."},
		{"thinking_level", "string", "Legacy reasoning effort: \"off\", \"low\", \"medium\", \"high\"."},
		{"reasoning_config", "object", "Reasoning policy JSON. Common keys: override_mode (inherit|custom), effort (off|auto|none|minimal|low|medium|high|xhigh), fallback (downgrade|off|provider_default)."},
		{"tools_config", "object", "Tools policy JSON. Common shape: {\"allow\": [\"tool_name\", ...], \"deny\": [...]}."},
		{"sandbox_config", "object", "Sandbox configuration JSON."},
		{"subagents_config", "object", "Subagent spawning configuration JSON."},
		{"memory_config", "object", "Memory subsystem config JSON. Common: {\"enabled\": true}."},
		{"compaction_config", "object", "Conversation compaction config JSON."},
		{"context_pruning", "object", "Context pruning config JSON."},
		{"other_config", "object", "Misc agent config bag (v3 flags, tts_params, etc.)."},
		{"kg_dedup_config", "object", "Knowledge graph dedup config JSON."},
		{"workspace_sharing", "object", "Workspace sharing config JSON."},
		{"chatgpt_oauth_routing", "object", "ChatGPT OAuth pool routing config JSON."},
		{"shell_deny_groups", "object", "Shell command deny-pattern groups JSON."},
		{"self_evolve", "boolean", "Allow agent to self-update SOUL.md. Predefined agents only."},
		{"skill_evolve", "boolean", "Allow agent to create skills via skill_manage."},
		{"skill_nudge_interval", "integer", "Skill nudge cadence (turns)."},
		{"is_default", "boolean", "Mark as tenant default agent. Setting true clears the flag on other agents automatically."},
		{"budget_monthly_cents", "integer", "Monthly spend cap in cents. null/omitted = unlimited."},
		{"workspace", "string", "Filesystem workspace path. Usually left alone — set during create."},
		{"emoji", "string", "Dashboard badge emoji."},
		{"agent_description", "string", "Short description used by the summoner. Updating this here does NOT trigger a re-summon; use the /v1/agents/{id}/regenerate endpoint for that."},
		{"frontmatter", "string", "1-2 sentence expertise summary (≤200 chars) used for delegation discovery."},
	}
	for _, sp := range specs {
		props[sp.name] = map[string]any{
			"type":        sp.typ,
			"description": sp.desc,
		}
	}

	return map[string]any{
		"type":       "object",
		"required":   []string{"action", "agent_id"},
		"properties": props,
	}
}

func (t *AgentConfigTool) Execute(ctx context.Context, args map[string]any) *Result {
	if t.agents == nil {
		return ErrorResult("agent_config: agent store not wired")
	}
	action, _ := args["action"].(string)
	if action == "" {
		return ErrorResult("action parameter is required")
	}
	switch action {
	case "read":
		return t.handleRead(ctx, args)
	case "update":
		return t.handleUpdate(ctx, args)
	default:
		return ErrorResult(fmt.Sprintf("unknown action %q. Valid: read, update", action))
	}
}

func (t *AgentConfigTool) resolveAgent(ctx context.Context, agentID string) (*store.AgentData, error) {
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

func (t *AgentConfigTool) handleRead(ctx context.Context, args map[string]any) *Result {
	agentID, _ := args["agent_id"].(string)
	ag, err := t.resolveAgent(ctx, agentID)
	if err != nil {
		return ErrorResult(err.Error())
	}
	out := agentConfigSnapshot(ag)
	body, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return ErrorResult(fmt.Sprintf("failed to marshal config: %v", err))
	}
	return NewResult(string(body))
}

func (t *AgentConfigTool) handleUpdate(ctx context.Context, args map[string]any) *Result {
	agentID, _ := args["agent_id"].(string)
	ag, err := t.resolveAgent(ctx, agentID)
	if err != nil {
		return ErrorResult(err.Error())
	}

	// Build the set of intended updates: copy of args minus action/agent_id,
	// keep only allowlisted keys, track silently-skipped keys for logging.
	updates := make(map[string]any, len(args))
	skipped := make([]string, 0)
	for k, v := range args {
		if k == "action" || k == "agent_id" {
			continue
		}
		if agentConfigMutableFields[k] {
			updates[k] = v
		} else {
			skipped = append(skipped, k)
		}
	}
	if len(skipped) > 0 {
		slog.Warn("agent_config.update: ignored non-mutable fields", "agent", ag.AgentKey, "skipped", skipped)
	}
	if len(updates) == 0 {
		return ErrorResult("update requires at least one mutable field (besides action and agent_id)")
	}

	// Per-field validation. Order: cheap checks first, JSONB parse last.
	if err := validateAgentConfigUpdates(updates); err != nil {
		return ErrorResult(err.Error())
	}

	// Snapshot before-values for the diff (only for keys we are touching).
	beforeSnap := agentConfigSnapshot(ag)
	before := make(map[string]any, len(updates))
	for k := range updates {
		if v, ok := beforeSnap[k]; ok {
			before[k] = v
		} else {
			before[k] = nil
		}
	}

	prevStatus := ag.Status
	prevDisplayName := ag.DisplayName
	prevAgentKey := ag.AgentKey

	if err := t.agents.Update(ctx, ag.ID, updates); err != nil {
		return ErrorResult(fmt.Sprintf("update failed: %v", err))
	}

	// Re-read for true post-state.
	updated, err := t.agents.GetByID(ctx, ag.ID)
	if err != nil {
		return ErrorResult(fmt.Sprintf("update succeeded but re-read failed: %v", err))
	}

	// IDENTITY.md sync when display_name changed.
	if newName, ok := updates["display_name"].(string); ok && newName != "" && newName != prevDisplayName {
		t.syncIdentityName(ctx, updated, newName)
	}

	// Cache invalidate: agent + bootstrap. Use the OLD agent_key for the
	// agent-cache key on rename (router cache holds tenantID:oldKey;
	// invalidating with the new key would miss).
	t.emitCacheInvalidate(bus.CacheKindAgent, prevAgentKey)
	if updated.AgentKey != prevAgentKey {
		t.emitCacheInvalidate(bus.CacheKindAgent, updated.AgentKey)
	}
	t.emitCacheInvalidate(bus.CacheKindBootstrap, ag.ID.String())

	// Status change broadcast.
	if newStatus, ok := updates["status"].(string); ok && newStatus != prevStatus {
		t.broadcastStatusChange(ctx, ag.ID, prevStatus, newStatus)
	}

	afterSnap := agentConfigSnapshot(updated)
	after := make(map[string]any, len(updates))
	fieldsChanged := make([]string, 0, len(updates))
	for k := range updates {
		if v, ok := afterSnap[k]; ok {
			after[k] = v
		} else {
			after[k] = nil
		}
		fieldsChanged = append(fieldsChanged, k)
	}

	resp := map[string]any{
		"agent_id":       updated.AgentKey,
		"agent_uuid":     updated.ID.String(),
		"fields_changed": fieldsChanged,
		"before":         before,
		"after":          after,
	}
	if len(skipped) > 0 {
		resp["ignored_fields"] = skipped
	}
	body, err := json.MarshalIndent(resp, "", "  ")
	if err != nil {
		return ErrorResult(fmt.Sprintf("failed to marshal response: %v", err))
	}
	return NewResult(string(body))
}

// syncIdentityName mirrors http.AgentsHandler.syncIdentityName for the
// agent-level IDENTITY.md only. Per-user copies (open agents) are skipped
// here — predefined agents share a single IDENTITY.md, and forge-created
// agents are predefined by default. If the agent is somehow open, the
// per-user copies stay stale until next bootstrap propagation.
func (t *AgentConfigTool) syncIdentityName(ctx context.Context, ag *store.AgentData, newName string) {
	existingContent := ""
	if dbFiles, err := t.agents.GetAgentContextFiles(ctx, ag.ID); err == nil {
		for _, f := range dbFiles {
			if f.FileName == bootstrap.IdentityFile {
				existingContent = f.Content
				break
			}
		}
	}
	newContent := bootstrap.UpdateIdentityField(existingContent, "Name", newName)
	if newContent == "" {
		newContent = "# Identity\nName: " + newName + "\n"
	}
	if err := t.agents.SetAgentContextFile(ctx, ag.ID, bootstrap.IdentityFile, newContent); err != nil {
		slog.Warn("agent_config.update: failed to sync IDENTITY.md name", "agent", ag.AgentKey, "error", err)
	}
}

func (t *AgentConfigTool) emitCacheInvalidate(kind, key string) {
	if t.msgBus == nil {
		return
	}
	t.msgBus.Broadcast(bus.Event{
		Name:    protocol.EventCacheInvalidate,
		Payload: bus.CacheInvalidatePayload{Kind: kind, Key: key},
	})
}

func (t *AgentConfigTool) broadcastStatusChange(ctx context.Context, agentID uuid.UUID, oldStatus, newStatus string) {
	if t.msgBus == nil {
		return
	}
	bus.BroadcastForTenant(t.msgBus, bus.EventAgentStatusChanged,
		store.TenantIDFromContext(ctx),
		bus.AgentStatusChangedPayload{
			AgentID:   agentID.String(),
			OldStatus: oldStatus,
			NewStatus: newStatus,
		})
}

// --- validation ---

func validateAgentConfigUpdates(updates map[string]any) error {
	if v, ok := updates["agent_key"]; ok {
		s, _ := v.(string)
		if s == "" {
			return fmt.Errorf("agent_key cannot be empty")
		}
		if !agentConfigSlugRe.MatchString(s) {
			return fmt.Errorf("agent_key %q does not match slug format ([a-z0-9] + hyphens, no leading/trailing hyphen)", s)
		}
	}
	if v, ok := updates["thinking_level"]; ok {
		s, _ := v.(string)
		if !agentConfigThinkingLevels[s] {
			return fmt.Errorf("thinking_level %q invalid; must be off|low|medium|high", s)
		}
	}
	if v, ok := updates["status"]; ok {
		s, _ := v.(string)
		if !agentConfigAllowedStatuses[s] {
			return fmt.Errorf("status %q invalid; settable values: active, summon_failed (summoning is reserved)", s)
		}
	}
	if v, ok := updates["reasoning_config"]; ok && v != nil {
		m, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("reasoning_config must be an object")
		}
		if eff, ok := m["effort"].(string); ok && eff != "" {
			if !agentConfigReasoningEfforts[eff] {
				return fmt.Errorf("reasoning_config.effort %q invalid; must be off|auto|none|minimal|low|medium|high|xhigh", eff)
			}
		}
	}
	for _, intField := range []string{"context_window", "max_tool_iterations", "max_tokens", "skill_nudge_interval", "budget_monthly_cents"} {
		if v, ok := updates[intField]; ok && v != nil {
			n, ok := asInt(v)
			if !ok {
				return fmt.Errorf("%s must be an integer, got %T", intField, v)
			}
			// budget_monthly_cents may be 0 (unlimited interpretation handled by store);
			// other size fields must be positive.
			if intField == "budget_monthly_cents" {
				if n < 0 {
					return fmt.Errorf("%s must be >= 0", intField)
				}
				continue
			}
			if n <= 0 {
				return fmt.Errorf("%s must be > 0", intField)
			}
		}
	}
	for field := range agentConfigJSONFields {
		if v, ok := updates[field]; ok && v != nil {
			if err := ensureJSONShape(field, v); err != nil {
				return err
			}
			if field == "other_config" {
				if m, ok := v.(map[string]any); ok {
					if err := store.ValidateV3Flags(m); err != nil {
						return fmt.Errorf("other_config: %w", err)
					}
				}
			}
		}
	}
	return nil
}

func ensureJSONShape(field string, v any) error {
	switch v.(type) {
	case map[string]any, []any:
		return nil
	case string:
		// Accept JSON-encoded string too: try to parse.
		s := v.(string)
		if s == "" {
			return nil
		}
		var parsed any
		if err := json.Unmarshal([]byte(s), &parsed); err != nil {
			return fmt.Errorf("%s: invalid JSON: %v", field, err)
		}
		return nil
	default:
		return fmt.Errorf("%s must be a JSON object/array (got %T)", field, v)
	}
}

func asInt(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case float64:
		// JSON unmarshal produces float64 for numbers. Reject non-integer floats.
		if x != float64(int(x)) {
			return 0, false
		}
		return int(x), true
	default:
		return 0, false
	}
}

// --- response shaping ---

// agentConfigSnapshot renders an agent's mutable config as a flat
// map[string]any suitable for JSON output. JSONB fields are unmarshaled to
// generic any so the response contains real nested objects, not strings.
func agentConfigSnapshot(ag *store.AgentData) map[string]any {
	out := map[string]any{
		"agent_id":             ag.AgentKey,
		"agent_uuid":           ag.ID.String(),
		"agent_type":           ag.AgentType,
		"agent_key":            ag.AgentKey,
		"display_name":         ag.DisplayName,
		"frontmatter":          ag.Frontmatter,
		"provider":             ag.Provider,
		"model":                ag.Model,
		"status":               ag.Status,
		"context_window":       ag.ContextWindow,
		"max_tool_iterations":  ag.MaxToolIterations,
		"max_tokens":           ag.MaxTokens,
		"thinking_level":       ag.ThinkingLevel,
		"emoji":                ag.Emoji,
		"agent_description":    ag.AgentDescription,
		"self_evolve":          ag.SelfEvolve,
		"skill_evolve":         ag.SkillEvolve,
		"skill_nudge_interval": ag.SkillNudgeInterval,
		"workspace":            ag.Workspace,
		"is_default":           ag.IsDefault,
		"restrict_to_workspace": ag.RestrictToWorkspace,
	}
	if ag.BudgetMonthlyCents != nil {
		out["budget_monthly_cents"] = *ag.BudgetMonthlyCents
	}
	for field, raw := range map[string]json.RawMessage{
		"tools_config":          ag.ToolsConfig,
		"sandbox_config":        ag.SandboxConfig,
		"subagents_config":      ag.SubagentsConfig,
		"memory_config":         ag.MemoryConfig,
		"compaction_config":     ag.CompactionConfig,
		"context_pruning":       ag.ContextPruning,
		"other_config":          ag.OtherConfig,
		"reasoning_config":      ag.ReasoningConfig,
		"workspace_sharing":     ag.WorkspaceSharing,
		"chatgpt_oauth_routing": ag.ChatGPTOAuthRouting,
		"shell_deny_groups":     ag.ShellDenyGroups,
		"kg_dedup_config":       ag.KGDedupConfig,
	} {
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var parsed any
		if err := json.Unmarshal(raw, &parsed); err != nil {
			// Keep raw on parse failure so the operator sees the actual stored bytes.
			out[field] = strings.TrimSpace(string(raw))
			continue
		}
		out[field] = parsed
	}
	return out
}
