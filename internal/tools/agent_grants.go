package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/pkg/protocol"
)

// agentGrantsSkillStore is the minimal SkillStore surface the tool uses.
// Production wires store.SkillManageStore (which satisfies this); tests
// can stub just these methods.
type agentGrantsSkillStore interface {
	ListSkills(ctx context.Context) []store.SkillInfo
	GetSkillByID(ctx context.Context, id uuid.UUID) (store.SkillInfo, bool)
	GrantToAgent(ctx context.Context, skillID, agentID uuid.UUID, version int, grantedBy string, canManage ...bool) error
	RevokeFromAgent(ctx context.Context, skillID, agentID uuid.UUID) error
	ListWithGrantStatus(ctx context.Context, agentID uuid.UUID) ([]store.SkillWithGrantStatus, error)
	BumpVersion()
}

// agentGrantsMCPStore is the minimal MCPServerStore surface the tool uses.
type agentGrantsMCPStore interface {
	ListServers(ctx context.Context) ([]store.MCPServerData, error)
	GetServer(ctx context.Context, id uuid.UUID) (*store.MCPServerData, error)
	GetServerByName(ctx context.Context, name string) (*store.MCPServerData, error)
	GrantToAgent(ctx context.Context, g *store.MCPAgentGrant) error
	RevokeFromAgent(ctx context.Context, serverID, agentID uuid.UUID) error
	ListAgentGrants(ctx context.Context, agentID uuid.UUID) ([]store.MCPAgentGrant, error)
}

// AgentGrantsTool manages skill and MCP-server grants on agents within the
// caller's tenant. Sub-actions cover the two grant kinds with parallel
// list/grant/revoke trios plus catalog enumeration helpers.
//
// Side effects replicated from HTTP handlers:
//   - Skill grant/revoke: skillStore.BumpVersion + cache invalidate
//     (CacheKindSkillGrants) + audit (skill.grant_changed).
//   - MCP grant/revoke: cache invalidate (CacheKindMCP) + audit
//     (mcp_server.agent_granted / agent_revoked).
//
// Access governed by tools_config.allow per-agent.
type AgentGrantsTool struct {
	agents store.AgentStore
	skills agentGrantsSkillStore
	mcp    agentGrantsMCPStore
	msgBus *bus.MessageBus
}

func NewAgentGrantsTool() *AgentGrantsTool { return &AgentGrantsTool{} }

func (t *AgentGrantsTool) SetAgentStore(s store.AgentStore)     { t.agents = s }
func (t *AgentGrantsTool) SetSkillStore(s agentGrantsSkillStore) { t.skills = s }
func (t *AgentGrantsTool) SetMCPStore(s agentGrantsMCPStore)     { t.mcp = s }
func (t *AgentGrantsTool) SetMessageBus(b *bus.MessageBus)       { t.msgBus = b }

func (t *AgentGrantsTool) Name() string { return "agent_grants" }

func (t *AgentGrantsTool) Description() string {
	return "Manage skill and MCP-server grants on agents within the tenant.\n\n" +
		"Actions: list (granted skills + MCP servers for an agent), grant_skill, revoke_skill, grant_mcp, revoke_mcp, list_skills_available (all skills in tenant), list_mcp_available (all MCP servers in tenant).\n\n" +
		"skill_id and server_id can be passed as UUID or as slug/name — the tool resolves both. " +
		"For skill grants, version defaults to 1 if omitted. " +
		"For MCP grants, optional tool_allow / tool_deny / config_overrides JSON objects scope which tools and config keys are exposed to the agent.\n\n" +
		"Use list_*_available to enumerate the catalog before granting. Use list to verify the effective grants on an agent after changes.\n\n" +
		"All operations are tenant-scoped: only entities within the caller's tenant are accessible."
}

func (t *AgentGrantsTool) Parameters() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"action"},
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"list", "grant_skill", "revoke_skill", "grant_mcp", "revoke_mcp", "list_skills_available", "list_mcp_available"},
				"description": "Operation to perform.",
			},
			"agent_id": map[string]any{
				"type":        "string",
				"description": "Target agent — agent_key (slug) or UUID. Required for list, grant_*, revoke_*.",
			},
			"skill_id": map[string]any{
				"type":        "string",
				"description": "Skill to grant/revoke. UUID or slug. Required for grant_skill and revoke_skill.",
			},
			"version": map[string]any{
				"type":        "integer",
				"description": "Skill version to grant. Defaults to 1. Used only by grant_skill.",
			},
			"server_id": map[string]any{
				"type":        "string",
				"description": "MCP server to grant/revoke. UUID or server name. Required for grant_mcp and revoke_mcp.",
			},
			"tool_allow": map[string]any{
				"type":        "array",
				"description": "Optional whitelist of tool names exposed by the MCP server to this agent. Used only by grant_mcp.",
			},
			"tool_deny": map[string]any{
				"type":        "array",
				"description": "Optional denylist of tool names. Used only by grant_mcp.",
			},
			"config_overrides": map[string]any{
				"type":        "object",
				"description": "Optional MCP config overrides JSON. Used only by grant_mcp.",
			},
		},
	}
}

func (t *AgentGrantsTool) Execute(ctx context.Context, args map[string]any) *Result {
	if t.agents == nil {
		return ErrorResult("agent_grants: agent store not wired")
	}
	action, _ := args["action"].(string)
	if action == "" {
		return ErrorResult("action parameter is required")
	}

	switch action {
	case "list":
		return t.handleList(ctx, args)
	case "grant_skill":
		return t.handleGrantSkill(ctx, args)
	case "revoke_skill":
		return t.handleRevokeSkill(ctx, args)
	case "grant_mcp":
		return t.handleGrantMCP(ctx, args)
	case "revoke_mcp":
		return t.handleRevokeMCP(ctx, args)
	case "list_skills_available":
		return t.handleListSkillsAvailable(ctx)
	case "list_mcp_available":
		return t.handleListMCPAvailable(ctx)
	default:
		return ErrorResult(fmt.Sprintf("unknown action %q. Valid: list, grant_skill, revoke_skill, grant_mcp, revoke_mcp, list_skills_available, list_mcp_available", action))
	}
}

// --- list ---

func (t *AgentGrantsTool) handleList(ctx context.Context, args map[string]any) *Result {
	agentID, _ := args["agent_id"].(string)
	ag, err := t.resolveAgent(ctx, agentID)
	if err != nil {
		return ErrorResult(err.Error())
	}

	resp := map[string]any{
		"agent_id":   ag.AgentKey,
		"agent_uuid": ag.ID.String(),
	}

	// Skills (granted only).
	if t.skills != nil {
		all, err := t.skills.ListWithGrantStatus(ctx, ag.ID)
		if err != nil {
			return ErrorResult(fmt.Sprintf("failed to list skill grants: %v", err))
		}
		granted := make([]map[string]any, 0)
		for _, s := range all {
			if !s.Granted {
				continue
			}
			entry := map[string]any{
				"skill_id":    s.ID,
				"slug":        s.Slug,
				"name":        s.Name,
				"description": s.Description,
				"version":     s.Version,
				"is_system":   s.IsSystem,
			}
			if s.PinnedVer != nil {
				entry["pinned_version"] = *s.PinnedVer
			}
			granted = append(granted, entry)
		}
		resp["skills"] = granted
	} else {
		resp["skills"] = []map[string]any{}
		resp["skills_note"] = "skill store not wired"
	}

	// MCP grants.
	if t.mcp != nil {
		grants, err := t.mcp.ListAgentGrants(ctx, ag.ID)
		if err != nil {
			return ErrorResult(fmt.Sprintf("failed to list MCP grants: %v", err))
		}
		mcpOut := make([]map[string]any, 0, len(grants))
		for _, g := range grants {
			entry := map[string]any{
				"grant_id":   g.ID.String(),
				"server_id":  g.ServerID.String(),
				"enabled":    g.Enabled,
				"granted_by": g.GrantedBy,
				"created_at": g.CreatedAt,
			}
			// Resolve server name for readability.
			if srv, _ := t.mcp.GetServer(ctx, g.ServerID); srv != nil {
				entry["server_name"] = srv.Name
			}
			if len(g.ToolAllow) > 0 {
				var parsed any
				if json.Unmarshal(g.ToolAllow, &parsed) == nil {
					entry["tool_allow"] = parsed
				}
			}
			if len(g.ToolDeny) > 0 {
				var parsed any
				if json.Unmarshal(g.ToolDeny, &parsed) == nil {
					entry["tool_deny"] = parsed
				}
			}
			if len(g.ConfigOverrides) > 0 {
				var parsed any
				if json.Unmarshal(g.ConfigOverrides, &parsed) == nil {
					entry["config_overrides"] = parsed
				}
			}
			mcpOut = append(mcpOut, entry)
		}
		resp["mcp"] = mcpOut
	} else {
		resp["mcp"] = []map[string]any{}
		resp["mcp_note"] = "mcp store not wired"
	}

	body, _ := json.MarshalIndent(resp, "", "  ")
	return NewResult(string(body))
}

// --- list_skills_available ---

func (t *AgentGrantsTool) handleListSkillsAvailable(ctx context.Context) *Result {
	if t.skills == nil {
		return ErrorResult("agent_grants: skill store not wired")
	}
	skills := t.skills.ListSkills(ctx)
	out := make([]map[string]any, 0, len(skills))
	for _, s := range skills {
		out = append(out, map[string]any{
			"skill_id":    s.ID,
			"slug":        s.Slug,
			"name":        s.Name,
			"description": s.Description,
			"version":     s.Version,
			"visibility":  s.Visibility,
			"is_system":   s.IsSystem,
		})
	}
	resp := map[string]any{
		"count":  len(out),
		"skills": out,
	}
	body, _ := json.MarshalIndent(resp, "", "  ")
	return NewResult(string(body))
}

// --- list_mcp_available ---

func (t *AgentGrantsTool) handleListMCPAvailable(ctx context.Context) *Result {
	if t.mcp == nil {
		return ErrorResult("agent_grants: mcp store not wired")
	}
	servers, err := t.mcp.ListServers(ctx)
	if err != nil {
		return ErrorResult(fmt.Sprintf("failed to list MCP servers: %v", err))
	}
	out := make([]map[string]any, 0, len(servers))
	for _, s := range servers {
		out = append(out, map[string]any{
			"server_id":   s.ID.String(),
			"name":        s.Name,
			"transport":   s.Transport,
			"enabled":     s.Enabled,
			"tool_prefix": s.ToolPrefix,
		})
	}
	resp := map[string]any{
		"count":   len(out),
		"servers": out,
	}
	body, _ := json.MarshalIndent(resp, "", "  ")
	return NewResult(string(body))
}

// --- grant_skill / revoke_skill ---

func (t *AgentGrantsTool) handleGrantSkill(ctx context.Context, args map[string]any) *Result {
	if t.skills == nil {
		return ErrorResult("agent_grants: skill store not wired")
	}
	agentID, _ := args["agent_id"].(string)
	skillRef, _ := args["skill_id"].(string)
	if skillRef == "" {
		return ErrorResult("skill_id is required for grant_skill")
	}
	ag, err := t.resolveAgent(ctx, agentID)
	if err != nil {
		return ErrorResult(err.Error())
	}
	sk, err := t.resolveSkill(ctx, skillRef)
	if err != nil {
		return ErrorResult(err.Error())
	}
	version := 1
	if v, ok := args["version"]; ok {
		if n, ok := asInt(v); ok && n > 0 {
			version = n
		}
	}

	grantedBy := store.UserIDFromContext(ctx)
	skillUUID, perr := uuid.Parse(sk.ID)
	if perr != nil {
		return ErrorResult(fmt.Sprintf("skill has invalid UUID %q: %v", sk.ID, perr))
	}
	if err := t.skills.GrantToAgent(ctx, skillUUID, ag.ID, version, grantedBy); err != nil {
		return ErrorResult(fmt.Sprintf("grant_skill failed: %v", err))
	}

	t.skills.BumpVersion()
	t.emitCacheInvalidate(bus.CacheKindSkillGrants, "")
	t.emitAudit(ctx, "skill.grant_changed", "skill", sk.ID)

	resp := map[string]any{
		"action":     "grant_skill",
		"agent_id":   ag.AgentKey,
		"skill_id":   sk.ID,
		"skill_slug": sk.Slug,
		"version":    version,
		"granted":    true,
	}
	body, _ := json.MarshalIndent(resp, "", "  ")
	return NewResult(string(body))
}

func (t *AgentGrantsTool) handleRevokeSkill(ctx context.Context, args map[string]any) *Result {
	if t.skills == nil {
		return ErrorResult("agent_grants: skill store not wired")
	}
	agentID, _ := args["agent_id"].(string)
	skillRef, _ := args["skill_id"].(string)
	if skillRef == "" {
		return ErrorResult("skill_id is required for revoke_skill")
	}
	ag, err := t.resolveAgent(ctx, agentID)
	if err != nil {
		return ErrorResult(err.Error())
	}
	sk, err := t.resolveSkill(ctx, skillRef)
	if err != nil {
		return ErrorResult(err.Error())
	}

	skillUUID, perr := uuid.Parse(sk.ID)
	if perr != nil {
		return ErrorResult(fmt.Sprintf("skill has invalid UUID %q: %v", sk.ID, perr))
	}
	if err := t.skills.RevokeFromAgent(ctx, skillUUID, ag.ID); err != nil {
		return ErrorResult(fmt.Sprintf("revoke_skill failed: %v", err))
	}

	t.skills.BumpVersion()
	t.emitCacheInvalidate(bus.CacheKindSkillGrants, "")
	t.emitAudit(ctx, "skill.grant_changed", "skill", sk.ID)

	resp := map[string]any{
		"action":     "revoke_skill",
		"agent_id":   ag.AgentKey,
		"skill_id":   sk.ID,
		"skill_slug": sk.Slug,
		"revoked":    true,
	}
	body, _ := json.MarshalIndent(resp, "", "  ")
	return NewResult(string(body))
}

// --- grant_mcp / revoke_mcp ---

func (t *AgentGrantsTool) handleGrantMCP(ctx context.Context, args map[string]any) *Result {
	if t.mcp == nil {
		return ErrorResult("agent_grants: mcp store not wired")
	}
	agentID, _ := args["agent_id"].(string)
	serverRef, _ := args["server_id"].(string)
	if serverRef == "" {
		return ErrorResult("server_id is required for grant_mcp")
	}
	ag, err := t.resolveAgent(ctx, agentID)
	if err != nil {
		return ErrorResult(err.Error())
	}
	srv, err := t.resolveMCPServer(ctx, serverRef)
	if err != nil {
		return ErrorResult(err.Error())
	}

	grant := &store.MCPAgentGrant{
		ServerID:  srv.ID,
		AgentID:   ag.ID,
		Enabled:   true,
		GrantedBy: store.UserIDFromContext(ctx),
	}
	if v, ok := args["tool_allow"]; ok && v != nil {
		raw, err := marshalAnyAsJSON(v)
		if err != nil {
			return ErrorResult(fmt.Sprintf("tool_allow: %v", err))
		}
		grant.ToolAllow = raw
	}
	if v, ok := args["tool_deny"]; ok && v != nil {
		raw, err := marshalAnyAsJSON(v)
		if err != nil {
			return ErrorResult(fmt.Sprintf("tool_deny: %v", err))
		}
		grant.ToolDeny = raw
	}
	if v, ok := args["config_overrides"]; ok && v != nil {
		raw, err := marshalAnyAsJSON(v)
		if err != nil {
			return ErrorResult(fmt.Sprintf("config_overrides: %v", err))
		}
		grant.ConfigOverrides = raw
	}

	if err := t.mcp.GrantToAgent(ctx, grant); err != nil {
		return ErrorResult(fmt.Sprintf("grant_mcp failed: %v", err))
	}

	t.emitCacheInvalidate(bus.CacheKindMCP, "")
	t.emitAudit(ctx, "mcp_server.agent_granted", "mcp_server", srv.ID.String())

	resp := map[string]any{
		"action":      "grant_mcp",
		"agent_id":    ag.AgentKey,
		"server_id":   srv.ID.String(),
		"server_name": srv.Name,
		"granted":     true,
	}
	body, _ := json.MarshalIndent(resp, "", "  ")
	return NewResult(string(body))
}

func (t *AgentGrantsTool) handleRevokeMCP(ctx context.Context, args map[string]any) *Result {
	if t.mcp == nil {
		return ErrorResult("agent_grants: mcp store not wired")
	}
	agentID, _ := args["agent_id"].(string)
	serverRef, _ := args["server_id"].(string)
	if serverRef == "" {
		return ErrorResult("server_id is required for revoke_mcp")
	}
	ag, err := t.resolveAgent(ctx, agentID)
	if err != nil {
		return ErrorResult(err.Error())
	}
	srv, err := t.resolveMCPServer(ctx, serverRef)
	if err != nil {
		return ErrorResult(err.Error())
	}

	if err := t.mcp.RevokeFromAgent(ctx, srv.ID, ag.ID); err != nil {
		return ErrorResult(fmt.Sprintf("revoke_mcp failed: %v", err))
	}

	t.emitCacheInvalidate(bus.CacheKindMCP, "")
	t.emitAudit(ctx, "mcp_server.agent_revoked", "mcp_server", srv.ID.String())

	resp := map[string]any{
		"action":      "revoke_mcp",
		"agent_id":    ag.AgentKey,
		"server_id":   srv.ID.String(),
		"server_name": srv.Name,
		"revoked":     true,
	}
	body, _ := json.MarshalIndent(resp, "", "  ")
	return NewResult(string(body))
}

// --- helpers ---

func (t *AgentGrantsTool) resolveAgent(ctx context.Context, agentID string) (*store.AgentData, error) {
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

// resolveSkill accepts UUID or slug. SkillInfo lookups by UUID are direct;
// slug lookup scans ListSkills (linear in tenant skill count — acceptable
// for the operator-driven low-frequency use case).
func (t *AgentGrantsTool) resolveSkill(ctx context.Context, ref string) (store.SkillInfo, error) {
	if ref == "" {
		return store.SkillInfo{}, fmt.Errorf("skill_id is required")
	}
	if id, err := uuid.Parse(ref); err == nil {
		if sk, ok := t.skills.GetSkillByID(ctx, id); ok {
			return sk, nil
		}
		return store.SkillInfo{}, fmt.Errorf("skill not found: %s", ref)
	}
	// Slug path
	all := t.skills.ListSkills(ctx)
	for _, s := range all {
		if s.Slug == ref {
			return s, nil
		}
	}
	return store.SkillInfo{}, fmt.Errorf("skill not found by slug: %s", ref)
}

// resolveMCPServer accepts UUID or name. GetServerByName is provided by the
// store; UUID path uses GetServer.
func (t *AgentGrantsTool) resolveMCPServer(ctx context.Context, ref string) (*store.MCPServerData, error) {
	if ref == "" {
		return nil, fmt.Errorf("server_id is required")
	}
	if id, err := uuid.Parse(ref); err == nil {
		srv, err := t.mcp.GetServer(ctx, id)
		if err != nil || srv == nil {
			return nil, fmt.Errorf("MCP server not found: %s", ref)
		}
		return srv, nil
	}
	srv, err := t.mcp.GetServerByName(ctx, ref)
	if err != nil || srv == nil {
		return nil, fmt.Errorf("MCP server not found by name: %s", ref)
	}
	return srv, nil
}

func (t *AgentGrantsTool) emitCacheInvalidate(kind, key string) {
	if t.msgBus == nil {
		return
	}
	t.msgBus.Broadcast(bus.Event{
		Name:    protocol.EventCacheInvalidate,
		Payload: bus.CacheInvalidatePayload{Kind: kind, Key: key},
	})
}

func (t *AgentGrantsTool) emitAudit(ctx context.Context, action, entityType, entityID string) {
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
			EntityType: entityType,
			EntityID:   entityID,
			TenantID:   store.TenantIDFromContext(ctx),
		},
	})
}

// resolveSkillSlugToID is an unused helper kept for future use if upstream
// adds a direct slug lookup. Currently slug resolution uses ListSkills.
var _ = strings.TrimSpace
