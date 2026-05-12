package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/hooks"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// agentHooksMutableFields lists hook columns the tool allows to be updated.
// Mirrors the WS handler's allowlist intent: protected provenance columns
// (source, version, created_by, tenant_id, id) and the agent_ids junction
// (use the set_agents action instead) are intentionally absent.
var agentHooksMutableFields = map[string]bool{
	"name":         true,
	"matcher":      true,
	"if_expr":      true,
	"timeout_ms":   true,
	"on_timeout":   true,
	"priority":     true,
	"enabled":      true,
	"config":       true,
	"handler_type": true,
	"event":        true,
	"metadata":     true,
}

var agentHooksValidEvents = map[hooks.HookEvent]bool{
	hooks.EventSessionStart:     true,
	hooks.EventUserPromptSubmit: true,
	hooks.EventPreToolUse:       true,
	hooks.EventPostToolUse:      true,
	hooks.EventStop:             true,
	hooks.EventSubagentStart:    true,
	hooks.EventSubagentStop:     true,
}

var agentHooksValidHandlerTypes = map[hooks.HandlerType]bool{
	hooks.HandlerCommand: true,
	hooks.HandlerHTTP:    true,
	hooks.HandlerPrompt:  true,
	hooks.HandlerScript:  true,
}

var agentHooksValidOnTimeout = map[hooks.Decision]bool{
	hooks.DecisionAllow: true,
	hooks.DecisionBlock: true,
}

// AgentHooksTool manages tenant- and agent-scope hooks via the hooks
// subsystem. Global hooks are blocked — they require master scope which
// admin agents are not granted.
//
// Sub-actions: list, get, create, update, delete, toggle, set_agents.
type AgentHooksTool struct {
	agents store.AgentStore
	hooks  hooks.HookStore
}

func NewAgentHooksTool() *AgentHooksTool { return &AgentHooksTool{} }

func (t *AgentHooksTool) SetAgentStore(s store.AgentStore) { t.agents = s }
func (t *AgentHooksTool) SetHookStore(s hooks.HookStore)   { t.hooks = s }

func (t *AgentHooksTool) Name() string { return "agent_hooks" }

func (t *AgentHooksTool) Description() string {
	return "Manage agent and tenant scope hooks (pre/post tool use, session_start, user_prompt_submit, stop, subagent_start/stop). Global-scope hooks are blocked — they require master scope.\n\n" +
		"Actions: list (filter by agent_id / event / scope), get (full hook by ID), create, update (partial patch), delete, toggle (enable/disable shortcut), set_agents (replace agent assignment for scope=agent hooks).\n\n" +
		"Handler types: command (exec a shell command with event JSON on stdin), http (POST event payload to URL), prompt (run a mini-LLM validation against event), script (embedded JS — builtin only). The config map shape depends on handler_type; runtime rejects malformed.\n\n" +
		"For scope=agent: pass agent_ids (array of agent_key or UUID) to bind the hook to specific agents. The Create call atomically populates the hook_agents junction.\n\n" +
		"matcher and if_expr filters narrow which tool_use events trigger the hook: matcher is a regex on tool_name (pre/post_tool_use only); if_expr is a CEL expression on tool_input.\n\n" +
		"timeout_ms is the wall-clock budget per hook invocation (default 5000). on_timeout chooses fail-closed (block) or fail-open (allow). priority sorts execution order higher-first."
}

func (t *AgentHooksTool) Parameters() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"action"},
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"list", "get", "create", "update", "delete", "toggle", "set_agents"},
				"description": "Operation to perform.",
			},

			// Identifiers
			"hook_id": map[string]any{
				"type":        "string",
				"description": "Hook UUID. Required for get, update, delete, toggle, set_agents.",
			},
			"agent_id": map[string]any{
				"type":        "string",
				"description": "Agent agent_key or UUID. Optional for list (filter hooks bound to this agent).",
			},

			// Create / update payload fields
			"event": map[string]any{
				"type":        "string",
				"enum":        []string{"session_start", "user_prompt_submit", "pre_tool_use", "post_tool_use", "stop", "subagent_start", "subagent_stop"},
				"description": "Hook event. Required for create.",
			},
			"handler_type": map[string]any{
				"type":        "string",
				"enum":        []string{"command", "http", "prompt", "script"},
				"description": "Handler type. Required for create. script is builtin-only.",
			},
			"scope": map[string]any{
				"type":        "string",
				"enum":        []string{"tenant", "agent"},
				"description": "Hook scope. Required for create. global is blocked.",
			},
			"config": map[string]any{
				"type":        "object",
				"description": "Handler-specific config map. Required for create. Shape depends on handler_type: command:{command:string}, http:{url:string,...}, prompt:{prompt:string,model:string,...}, script:{...}.",
			},
			"name":       map[string]any{"type": "string", "description": "Human-readable hook name."},
			"matcher":    map[string]any{"type": "string", "description": "Regex against tool_name. Only for pre/post_tool_use."},
			"if_expr":    map[string]any{"type": "string", "description": "CEL expression on tool_input."},
			"timeout_ms": map[string]any{"type": "integer", "description": "Wall-clock budget per invocation. Default 5000."},
			"on_timeout": map[string]any{"type": "string", "enum": []string{"block", "allow"}, "description": "Decision on timeout. Default block (fail-closed)."},
			"priority":   map[string]any{"type": "integer", "description": "Execution order; higher first. Default 0."},
			"enabled":    map[string]any{"type": "boolean", "description": "Whether the hook fires. Default true."},
			"metadata":   map[string]any{"type": "object", "description": "Misc metadata bag."},

			// Agent binding
			"agent_ids": map[string]any{
				"type":        "array",
				"description": "List of agent_key or UUID. Required for scope=agent on create. Used by set_agents to replace the junction.",
			},

			// list filter
			"enabled_only": map[string]any{
				"type":        "boolean",
				"description": "List filter: include only enabled hooks. Default false (all).",
			},

			// update payload
			"updates": map[string]any{
				"type":        "object",
				"description": "Update payload: map of field→value to patch. Required for update. Allowed: name, matcher, if_expr, timeout_ms, on_timeout, priority, enabled, config, handler_type, event, metadata.",
			},
		},
	}
}

func (t *AgentHooksTool) Execute(ctx context.Context, args map[string]any) *Result {
	if t.hooks == nil {
		return ErrorResult("agent_hooks: hook store not wired")
	}
	if t.agents == nil {
		return ErrorResult("agent_hooks: agent store not wired")
	}
	action, _ := args["action"].(string)
	if action == "" {
		return ErrorResult("action parameter is required")
	}
	switch action {
	case "list":
		return t.handleList(ctx, args)
	case "get":
		return t.handleGet(ctx, args)
	case "create":
		return t.handleCreate(ctx, args)
	case "update":
		return t.handleUpdate(ctx, args)
	case "delete":
		return t.handleDelete(ctx, args)
	case "toggle":
		return t.handleToggle(ctx, args)
	case "set_agents":
		return t.handleSetAgents(ctx, args)
	default:
		return ErrorResult(fmt.Sprintf("unknown action %q. Valid: list, get, create, update, delete, toggle, set_agents", action))
	}
}

// --- list / get ---

func (t *AgentHooksTool) handleList(ctx context.Context, args map[string]any) *Result {
	filter := hooks.ListFilter{}
	if v, ok := args["enabled_only"].(bool); ok && v {
		flag := true
		filter.Enabled = &flag
	}
	if v, ok := args["event"].(string); ok && v != "" {
		ev := hooks.HookEvent(v)
		filter.Event = &ev
	}
	if v, ok := args["scope"].(string); ok && v != "" {
		sc := hooks.Scope(v)
		filter.Scope = &sc
	}
	if v, ok := args["agent_id"].(string); ok && v != "" {
		ag, err := t.resolveAgent(ctx, v)
		if err != nil {
			return ErrorResult(err.Error())
		}
		filter.AgentID = &ag.ID
	}

	list, err := t.hooks.List(ctx, filter)
	if err != nil {
		return ErrorResult(fmt.Sprintf("list failed: %v", err))
	}
	out := make([]map[string]any, 0, len(list))
	for _, h := range list {
		out = append(out, hookSummary(h))
	}
	resp := map[string]any{
		"count": len(out),
		"hooks": out,
	}
	body, _ := json.MarshalIndent(resp, "", "  ")
	return NewResult(string(body))
}

func (t *AgentHooksTool) handleGet(ctx context.Context, args map[string]any) *Result {
	hookID, _ := args["hook_id"].(string)
	id, err := uuid.Parse(hookID)
	if err != nil {
		return ErrorResult("hook_id must be a UUID")
	}
	h, err := t.hooks.GetByID(ctx, id)
	if err != nil {
		return ErrorResult(fmt.Sprintf("get failed: %v", err))
	}
	if h == nil {
		return ErrorResult(fmt.Sprintf("hook not found: %s", hookID))
	}
	// Augment with agent_ids from junction
	agentIDs, _ := t.hooks.GetHookAgents(ctx, id)
	full := hookFull(*h, agentIDs)
	body, _ := json.MarshalIndent(full, "", "  ")
	return NewResult(string(body))
}

// --- create ---

func (t *AgentHooksTool) handleCreate(ctx context.Context, args map[string]any) *Result {
	cfg, err := t.buildHookConfig(ctx, args)
	if err != nil {
		return ErrorResult(err.Error())
	}

	if cfg.Scope == hooks.ScopeGlobal {
		return ErrorResult("scope=global is blocked; this tool only handles tenant and agent scope. Global hooks require master-scope and must be created out-of-band.")
	}

	// Set createdBy from context user.
	if uid := store.UserIDFromContext(ctx); uid != "" {
		if parsed, err := uuid.Parse(uid); err == nil {
			cfg.CreatedBy = &parsed
		}
	}

	id, err := t.hooks.Create(ctx, *cfg)
	if err != nil {
		return ErrorResult(fmt.Sprintf("create failed: %v", err))
	}

	// Re-read to return canonical post-state (includes server-assigned defaults
	// and agent_ids from junction).
	stored, _ := t.hooks.GetByID(ctx, id)
	agentIDs, _ := t.hooks.GetHookAgents(ctx, id)
	var resp map[string]any
	if stored != nil {
		resp = hookFull(*stored, agentIDs)
	} else {
		resp = map[string]any{"hook_id": id.String()}
	}
	resp["created"] = true
	body, _ := json.MarshalIndent(resp, "", "  ")
	return NewResult(string(body))
}

// --- update / toggle ---

func (t *AgentHooksTool) handleUpdate(ctx context.Context, args map[string]any) *Result {
	hookID, _ := args["hook_id"].(string)
	id, err := uuid.Parse(hookID)
	if err != nil {
		return ErrorResult("hook_id must be a UUID")
	}
	raw, ok := args["updates"].(map[string]any)
	if !ok || len(raw) == 0 {
		return ErrorResult("update requires non-empty updates map")
	}
	clean := make(map[string]any, len(raw))
	skipped := make([]string, 0)
	for k, v := range raw {
		if agentHooksMutableFields[k] {
			clean[k] = v
		} else {
			skipped = append(skipped, k)
		}
	}
	if len(clean) == 0 {
		return ErrorResult("update: no mutable fields in updates (forbidden or unknown keys: " + joinStrings(skipped, ", ") + ")")
	}
	// Per-field validation
	if v, ok := clean["event"]; ok {
		if s, _ := v.(string); !agentHooksValidEvents[hooks.HookEvent(s)] {
			return ErrorResult(fmt.Sprintf("event %q invalid", s))
		}
	}
	if v, ok := clean["handler_type"]; ok {
		if s, _ := v.(string); !agentHooksValidHandlerTypes[hooks.HandlerType(s)] {
			return ErrorResult(fmt.Sprintf("handler_type %q invalid", s))
		}
	}
	if v, ok := clean["on_timeout"]; ok {
		if s, _ := v.(string); !agentHooksValidOnTimeout[hooks.Decision(s)] {
			return ErrorResult(fmt.Sprintf("on_timeout %q invalid; must be block or allow", s))
		}
	}
	if v, ok := clean["timeout_ms"]; ok {
		if n, ok := asInt(v); !ok || n <= 0 {
			return ErrorResult("timeout_ms must be a positive integer")
		}
	}

	if err := t.hooks.Update(ctx, id, clean); err != nil {
		return ErrorResult(fmt.Sprintf("update failed: %v", err))
	}
	stored, _ := t.hooks.GetByID(ctx, id)
	agentIDs, _ := t.hooks.GetHookAgents(ctx, id)
	resp := map[string]any{
		"hook_id":        id.String(),
		"fields_changed": sortedKeysAny(clean),
		"updated":        true,
	}
	if stored != nil {
		resp["after"] = hookFull(*stored, agentIDs)
	}
	if len(skipped) > 0 {
		resp["ignored_fields"] = skipped
	}
	body, _ := json.MarshalIndent(resp, "", "  ")
	return NewResult(string(body))
}

func (t *AgentHooksTool) handleToggle(ctx context.Context, args map[string]any) *Result {
	hookID, _ := args["hook_id"].(string)
	id, err := uuid.Parse(hookID)
	if err != nil {
		return ErrorResult("hook_id must be a UUID")
	}
	v, ok := args["enabled"].(bool)
	if !ok {
		return ErrorResult("toggle requires enabled (boolean)")
	}
	if err := t.hooks.Update(ctx, id, map[string]any{"enabled": v}); err != nil {
		return ErrorResult(fmt.Sprintf("toggle failed: %v", err))
	}
	resp := map[string]any{
		"hook_id": id.String(),
		"enabled": v,
		"toggled": true,
	}
	body, _ := json.MarshalIndent(resp, "", "  ")
	return NewResult(string(body))
}

// --- delete ---

func (t *AgentHooksTool) handleDelete(ctx context.Context, args map[string]any) *Result {
	hookID, _ := args["hook_id"].(string)
	id, err := uuid.Parse(hookID)
	if err != nil {
		return ErrorResult("hook_id must be a UUID")
	}
	if err := t.hooks.Delete(ctx, id); err != nil {
		return ErrorResult(fmt.Sprintf("delete failed: %v", err))
	}
	resp := map[string]any{
		"hook_id": id.String(),
		"deleted": true,
	}
	body, _ := json.MarshalIndent(resp, "", "  ")
	return NewResult(string(body))
}

// --- set_agents ---

func (t *AgentHooksTool) handleSetAgents(ctx context.Context, args map[string]any) *Result {
	hookID, _ := args["hook_id"].(string)
	id, err := uuid.Parse(hookID)
	if err != nil {
		return ErrorResult("hook_id must be a UUID")
	}
	rawIDs, ok := args["agent_ids"].([]any)
	if !ok {
		return ErrorResult("set_agents requires agent_ids array")
	}
	uuids, err := t.resolveAgentIDs(ctx, rawIDs)
	if err != nil {
		return ErrorResult(err.Error())
	}
	if err := t.hooks.SetHookAgents(ctx, id, uuids); err != nil {
		return ErrorResult(fmt.Sprintf("set_agents failed: %v", err))
	}
	agentUUIDs := make([]string, 0, len(uuids))
	for _, u := range uuids {
		agentUUIDs = append(agentUUIDs, u.String())
	}
	resp := map[string]any{
		"hook_id":   id.String(),
		"agent_ids": agentUUIDs,
		"count":     len(agentUUIDs),
		"updated":   true,
	}
	body, _ := json.MarshalIndent(resp, "", "  ")
	return NewResult(string(body))
}

// --- helpers ---

// buildHookConfig assembles a HookConfig from args for create. Validates
// required fields and enum values. Resolves agent_ids to UUIDs.
func (t *AgentHooksTool) buildHookConfig(ctx context.Context, args map[string]any) (*hooks.HookConfig, error) {
	cfg := &hooks.HookConfig{
		TenantID:  store.TenantIDFromContext(ctx),
		Config:    map[string]any{},
		Metadata:  map[string]any{},
		TimeoutMS: 5000,
		OnTimeout: hooks.DecisionBlock,
		Enabled:   true,
	}

	evStr, _ := args["event"].(string)
	if evStr == "" {
		return nil, fmt.Errorf("event is required")
	}
	ev := hooks.HookEvent(evStr)
	if !agentHooksValidEvents[ev] {
		return nil, fmt.Errorf("event %q invalid", evStr)
	}
	cfg.Event = ev

	htStr, _ := args["handler_type"].(string)
	if htStr == "" {
		return nil, fmt.Errorf("handler_type is required")
	}
	ht := hooks.HandlerType(htStr)
	if !agentHooksValidHandlerTypes[ht] {
		return nil, fmt.Errorf("handler_type %q invalid", htStr)
	}
	cfg.HandlerType = ht

	scStr, _ := args["scope"].(string)
	if scStr == "" {
		return nil, fmt.Errorf("scope is required")
	}
	cfg.Scope = hooks.Scope(scStr)

	cfgMap, ok := args["config"].(map[string]any)
	if !ok || len(cfgMap) == 0 {
		return nil, fmt.Errorf("config map is required for create")
	}
	cfg.Config = cfgMap

	if v, ok := args["name"].(string); ok {
		cfg.Name = v
	}
	if v, ok := args["matcher"].(string); ok {
		cfg.Matcher = v
	}
	if v, ok := args["if_expr"].(string); ok {
		cfg.IfExpr = v
	}
	if v, ok := args["timeout_ms"]; ok {
		if n, ok := asInt(v); ok && n > 0 {
			cfg.TimeoutMS = n
		} else {
			return nil, fmt.Errorf("timeout_ms must be a positive integer")
		}
	}
	if v, ok := args["on_timeout"].(string); ok && v != "" {
		d := hooks.Decision(v)
		if !agentHooksValidOnTimeout[d] {
			return nil, fmt.Errorf("on_timeout %q invalid; must be block or allow", v)
		}
		cfg.OnTimeout = d
	}
	if v, ok := args["priority"]; ok {
		if n, ok := asInt(v); ok {
			cfg.Priority = n
		}
	}
	if v, ok := args["enabled"].(bool); ok {
		cfg.Enabled = v
	}
	if v, ok := args["metadata"].(map[string]any); ok {
		cfg.Metadata = v
	}

	if cfg.Scope == hooks.ScopeAgent {
		raw, ok := args["agent_ids"].([]any)
		if !ok || len(raw) == 0 {
			return nil, fmt.Errorf("agent_ids required for scope=agent")
		}
		uuids, err := t.resolveAgentIDs(ctx, raw)
		if err != nil {
			return nil, err
		}
		cfg.AgentIDs = uuids
	}

	return cfg, nil
}

func (t *AgentHooksTool) resolveAgent(ctx context.Context, agentID string) (*store.AgentData, error) {
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

func (t *AgentHooksTool) resolveAgentIDs(ctx context.Context, raw []any) ([]uuid.UUID, error) {
	out := make([]uuid.UUID, 0, len(raw))
	for i, item := range raw {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("agent_ids[%d]: must be string", i)
		}
		ag, err := t.resolveAgent(ctx, s)
		if err != nil {
			return nil, fmt.Errorf("agent_ids[%d]: %v", i, err)
		}
		out = append(out, ag.ID)
	}
	return out, nil
}

// hookSummary returns the compact list-view of a hook.
func hookSummary(h hooks.HookConfig) map[string]any {
	out := map[string]any{
		"hook_id":      h.ID.String(),
		"name":         h.Name,
		"event":        string(h.Event),
		"handler_type": string(h.HandlerType),
		"scope":        string(h.Scope),
		"enabled":      h.Enabled,
		"priority":     h.Priority,
		"source":       h.Source,
		"version":      h.Version,
	}
	if h.Matcher != "" {
		out["matcher"] = h.Matcher
	}
	return out
}

// hookFull returns the full hook including config + agent_ids junction.
func hookFull(h hooks.HookConfig, agentIDs []uuid.UUID) map[string]any {
	out := hookSummary(h)
	out["config"] = h.Config
	out["timeout_ms"] = h.TimeoutMS
	out["on_timeout"] = string(h.OnTimeout)
	out["created_at"] = h.CreatedAt.Format(time.RFC3339)
	out["updated_at"] = h.UpdatedAt.Format(time.RFC3339)
	if h.IfExpr != "" {
		out["if_expr"] = h.IfExpr
	}
	if len(h.Metadata) > 0 {
		out["metadata"] = h.Metadata
	}
	if h.CreatedBy != nil {
		out["created_by"] = h.CreatedBy.String()
	}
	if len(agentIDs) > 0 {
		ids := make([]string, 0, len(agentIDs))
		for _, u := range agentIDs {
			ids = append(ids, u.String())
		}
		out["agent_ids"] = ids
	}
	return out
}

func sortedKeysAny(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	return keys
}

func joinStrings(s []string, sep string) string {
	if len(s) == 0 {
		return ""
	}
	out := s[0]
	for i := 1; i < len(s); i++ {
		out += sep + s[i]
	}
	return out
}
