package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"mime"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/tools"
)

// BridgeToolNamesPublic is the set of GoClaw tools exposed to every agent via
// the MCP bridge regardless of tools_config.allow. General-purpose actions
// without cross-agent or destructive scope.
//
// Excluded from the bridge entirely: spawn (agent loop) and create_forum_topic
// (channels owned by the agent loop, not the CLI).
var BridgeToolNamesPublic = map[string]bool{
	// Filesystem
	"read_file":  true,
	"write_file": true,
	"list_files": true,
	"edit":       true,
	"exec":       true,
	// Web
	"web_search": true,
	"web_fetch":  true,
	// Memory & knowledge (read-only)
	"memory_search":          true,
	"memory_get":             true,
	"skill_search":           true,
	"knowledge_graph_search": true,
	// Media
	"read_image":   true,
	"read_audio":   true,
	"read_video":   true,
	"create_image": true,
	"tts":          true,
	// Browser automation
	"browser": true,
	// Scheduler
	"cron": true,
	// Messaging (send text/files to channels)
	"message": true,
	// Sessions (read + send)
	"sessions_list":    true,
	"session_status":   true,
	"sessions_history": true,
	"sessions_send":    true,
	// Team tools (context from X-Agent-ID/X-Channel/X-Chat-ID headers)
	"team_tasks": true,
}

// BridgeToolNamesAdminOptIn is the set of cross-agent or catalog-mutating tools
// that are NOT exposed by default. An agent gets them only when its
// tools_config.allow explicitly contains the tool name.
//
// This is the opt-in surface for admin-class agents like forge. New agents
// receive none of these unless an operator (or forge itself) grants them.
var BridgeToolNamesAdminOptIn = map[string]bool{
	// Cross-agent admin (fork: agent_* tools)
	"agent_context_files": true,
	"agent_config":        true,
	"agent_provision":     true,
	"agent_grants":        true,
	"agent_hooks":         true,
	"agent_telegram":      true,
	// Skill catalog mutations
	"publish_skill": true,
	"skill_manage":  true,
	// Knowledge graph mutation (read counterpart is public)
	"knowledge_graph_mutate": true,
}

// BridgeAgentLookup is the minimal AgentStore subset the bridge needs.
// Declared locally so tests can stub it without implementing the full
// store.AgentStore interface (~30 methods). Any store.AgentStore satisfies
// this implicitly.
type BridgeAgentLookup interface {
	GetByIDUnscoped(ctx context.Context, id uuid.UUID) (*store.AgentData, error)
}

// agentAllowedBridgeTools returns the set of tool names the agent in ctx may
// invoke via the bridge. Always includes the public set; admin opt-in tools
// are added only when present in the agent's tools_config.allow.
//
// Fail-safe: if lookup is nil, agent ID is absent, lookup fails, or the
// agent has no allow-list, only public tools are returned. Admin tools never
// leak to an agent that hasn't been explicitly granted them.
func agentAllowedBridgeTools(ctx context.Context, lookup BridgeAgentLookup) map[string]bool {
	allowed := make(map[string]bool, len(BridgeToolNamesPublic))
	for n := range BridgeToolNamesPublic {
		allowed[n] = true
	}
	if lookup == nil {
		return allowed
	}
	id := store.AgentIDFromContext(ctx)
	if id == uuid.Nil {
		return allowed
	}
	ag, err := lookup.GetByIDUnscoped(ctx, id)
	if err != nil || ag == nil {
		return allowed
	}
	policy := ag.ParseToolsConfig()
	if policy == nil || len(policy.Allow) == 0 {
		return allowed
	}
	for _, name := range policy.Allow {
		if BridgeToolNamesAdminOptIn[name] {
			allowed[name] = true
		}
	}
	return allowed
}

// NewBridgeServer creates a StreamableHTTPServer that exposes GoClaw tools as
// MCP tools. Tools are registered from two sets:
//   - BridgeToolNamesPublic: available to every agent
//   - BridgeToolNamesAdminOptIn: available only when the agent's
//     tools_config.allow explicitly lists the tool
//
// Per-request filtering uses the agent UUID injected into context by the
// bridgeContextMiddleware (X-Agent-ID header, HMAC-protected).
//
// msgBus is optional; when non-nil, tools that produce media (deliver:true)
// will publish file attachments directly to the outbound bus.
//
// agentLookup is optional; when nil, only public tools are ever exposed.
func NewBridgeServer(reg *tools.Registry, version string, msgBus *bus.MessageBus, agentLookup BridgeAgentLookup) *mcpserver.StreamableHTTPServer {
	// Filter applied at tools/list. Hides admin tools from agents that haven't
	// opted into them via tools_config.allow.
	filter := func(ctx context.Context, ts []mcpgo.Tool) []mcpgo.Tool {
		allowed := agentAllowedBridgeTools(ctx, agentLookup)
		out := make([]mcpgo.Tool, 0, len(ts))
		for _, t := range ts {
			if allowed[t.Name] {
				out = append(out, t)
			}
		}
		return out
	}

	// Defense-in-depth at tools/call: even if a client cached an old tools/list
	// or fabricates a name, deny admin tools that aren't in the agent's allow.
	authMW := func(next mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
		return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			name := req.Params.Name
			if BridgeToolNamesPublic[name] {
				return next(ctx, req)
			}
			if BridgeToolNamesAdminOptIn[name] {
				allowed := agentAllowedBridgeTools(ctx, agentLookup)
				if !allowed[name] {
					slog.Warn("mcp.bridge: admin tool denied (not in agent allow-list)",
						"tool", name,
						"agent_id", store.AgentIDFromContext(ctx))
					return mcpgo.NewToolResultError("tool not allowed for this agent"), nil
				}
			}
			return next(ctx, req)
		}
	}

	srv := mcpserver.NewMCPServer("goclaw-bridge", version,
		mcpserver.WithToolCapabilities(false),
		mcpserver.WithToolFilter(filter),
		mcpserver.WithToolHandlerMiddleware(authMW),
	)

	register := func(set map[string]bool) int {
		var n int
		for name := range set {
			t, ok := reg.Get(name)
			if !ok {
				continue
			}
			srv.AddTool(convertToMCPTool(t), makeToolHandler(reg, name, msgBus))
			n++
		}
		return n
	}
	publicCount := register(BridgeToolNamesPublic)
	adminCount := register(BridgeToolNamesAdminOptIn)

	slog.Info("mcp.bridge: tools registered",
		"count", publicCount+adminCount,
		"public", publicCount,
		"admin_opt_in", adminCount)

	return mcpserver.NewStreamableHTTPServer(srv,
		mcpserver.WithStateLess(true),
	)
}

// convertToMCPTool converts a GoClaw tools.Tool into an mcp-go Tool.
func convertToMCPTool(t tools.Tool) mcpgo.Tool {
	schema, err := json.Marshal(t.Parameters())
	if err != nil {
		// Fallback: empty object schema
		schema = []byte(`{"type":"object"}`)
	}
	return mcpgo.NewToolWithRawSchema(t.Name(), t.Description(), schema)
}

// makeToolHandler creates a ToolHandlerFunc that delegates to the GoClaw tool registry.
// When msgBus is non-nil and a tool result contains Media paths, the handler publishes
// them as outbound media attachments so files reach the user (e.g. Telegram document).
func makeToolHandler(reg *tools.Registry, toolName string, msgBus *bus.MessageBus) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		args := req.GetArguments()

		// Pass routing context (channel, chatID, peerKind, sessionKey) so native
		// tools can access local_key, session_key etc. for forum topic routing.
		result := reg.ExecuteWithContext(ctx, toolName, args,
			tools.ToolChannelFromCtx(ctx),
			tools.ToolChatIDFromCtx(ctx),
			tools.ToolPeerKindFromCtx(ctx),
			tools.ToolSessionKeyFromCtx(ctx),
			nil,
		)

		if result.IsError {
			return mcpgo.NewToolResultError(result.ForLLM), nil
		}

		// Forward media files to the outbound bus so they reach the user as attachments.
		// This is necessary because Claude CLI processes tool results internally —
		// GoClaw's agent loop never sees result.Media from bridge tool calls.
		forwardMediaToOutbound(ctx, msgBus, toolName, result)

		return mcpgo.NewToolResultText(result.ForLLM), nil
	}
}

// forwardMediaToOutbound publishes media files from a tool result to the outbound bus.
func forwardMediaToOutbound(ctx context.Context, msgBus *bus.MessageBus, toolName string, result *tools.Result) {
	if msgBus == nil || len(result.Media) == 0 {
		return
	}
	channel := tools.ToolChannelFromCtx(ctx)
	chatID := tools.ToolChatIDFromCtx(ctx)
	if channel == "" || chatID == "" {
		slog.Debug("mcp.bridge: skipping media forward, missing channel context",
			"tool", toolName, "channel", channel, "chat_id", chatID)
		return
	}

	var attachments []bus.MediaAttachment
	for _, mf := range result.Media {
		ct := mf.MimeType
		if ct == "" {
			ct = mimeFromExt(filepath.Ext(mf.Path))
		}
		attachments = append(attachments, bus.MediaAttachment{
			URL:         mf.Path,
			ContentType: ct,
		})
	}

	peerKind := tools.ToolPeerKindFromCtx(ctx)
	var meta map[string]string
	if peerKind == "group" {
		meta = map[string]string{"group_id": chatID}
	}
	msgBus.PublishOutbound(bus.OutboundMessage{
		Channel:  channel,
		ChatID:   chatID,
		Media:    attachments,
		Metadata: meta,
	})
	slog.Debug("mcp.bridge: forwarded media to outbound bus",
		"tool", toolName, "channel", channel, "files", len(attachments))
}

// mimeFromExt returns a MIME type for a file extension.
// Uses Go stdlib first, falls back to a small map for types not reliably
// handled by mime.TypeByExtension on all platforms (e.g. .opus, .webp).
func mimeFromExt(ext string) string {
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	switch strings.ToLower(ext) {
	case ".webp":
		return "image/webp"
	case ".opus":
		return "audio/ogg"
	case ".md":
		return "text/markdown"
	default:
		return "application/octet-stream"
	}
}
