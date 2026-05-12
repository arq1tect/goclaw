package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/pkg/protocol"
)

// agentTelegramAPIBaseDefault is the production Telegram Bot API URL.
// Overridable for tests via SetAPIBase.
const agentTelegramAPIBaseDefault = "https://api.telegram.org"

// agentTelegramHTTPTimeout caps each outbound Telegram API call. Bot API
// methods are generally fast (<1s); 10s leaves slack for slow networks.
const agentTelegramHTTPTimeout = 10 * time.Second

// AgentTelegramTool orchestrates the Telegram Managed Bots flow (Bot API
// 9.6, April 2026): given a parent bot that has Bot-Management-Mode
// enabled, the operator can be sent a one-tap deep link to create a new
// child bot, which the tool then resolves to a token and registers as a
// new Telegram channel for a target agent.
//
// Sub-actions:
//   - list_channels: list Telegram channels in the tenant (optionally filtered by agent).
//   - request_managed_bot: send the operator a deep-link message via the parent bot.
//   - poll_managed_bot_token: poll Telegram getManagedBotToken; on success register child as new channel.
//   - unlink: delete a Telegram channel (channel_id or channel_name).
//
// The parent bot's token is read from the channels store (encrypted at
// rest, decrypted transparently by the store). The new bot's token is
// encrypted via the same store path when persisted.
//
// MVP design: no inbound channels-layer extension. The
// poll_managed_bot_token action returns "not yet" until Telegram has the
// token available; forge or the operator decides retry cadence.
type AgentTelegramTool struct {
	channels store.ChannelInstanceStore
	agents   store.AgentStore
	msgBus   *bus.MessageBus
	apiBase  string
	http     *http.Client
}

func NewAgentTelegramTool() *AgentTelegramTool {
	return &AgentTelegramTool{
		apiBase: agentTelegramAPIBaseDefault,
		http:    &http.Client{Timeout: agentTelegramHTTPTimeout},
	}
}

func (t *AgentTelegramTool) SetChannelStore(s store.ChannelInstanceStore) { t.channels = s }
func (t *AgentTelegramTool) SetAgentStore(s store.AgentStore)             { t.agents = s }
func (t *AgentTelegramTool) SetMessageBus(b *bus.MessageBus)              { t.msgBus = b }

// SetAPIBase overrides the Telegram API base URL. Used by tests to point
// at httptest.Server. Production should not call this.
func (t *AgentTelegramTool) SetAPIBase(base string) {
	if base != "" {
		t.apiBase = base
	}
}

// SetHTTPClient lets callers (or tests) inject a custom http.Client.
func (t *AgentTelegramTool) SetHTTPClient(c *http.Client) {
	if c != nil {
		t.http = c
	}
}

func (t *AgentTelegramTool) Name() string { return "agent_telegram" }

func (t *AgentTelegramTool) Description() string {
	return "Manage Telegram channels for agents via the Bot API 9.6 Managed Bots flow.\n\n" +
		"Actions: list_channels, request_managed_bot, poll_managed_bot_token, unlink.\n\n" +
		"request_managed_bot sends the operator a one-tap deep link (t.me/newbot/{parent_username}/{suggested_username}) through the parent bot. The operator taps in their Telegram client to confirm bot creation.\n\n" +
		"poll_managed_bot_token then calls Telegram's getManagedBotToken on the parent bot. While the operator has not yet tapped, this returns a polling-status response (no error). When the token is available, the tool encrypts and registers it as a new channel bound to the target agent.\n\n" +
		"Parent bot identification: pass parent_channel_id or parent_channel_name. Without either, the tool uses any Telegram channel attached to the calling agent (i.e. the agent invoking this tool acts as parent).\n\n" +
		"Naming convention for spawned bots is recommended as {agent_key_underscored}_ath_bot (operator may override). The tool does not enforce this; just pass suggested_username verbatim.\n\n" +
		"All operations tenant-scoped via the underlying stores."
}

func (t *AgentTelegramTool) Parameters() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"action"},
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"list_channels", "request_managed_bot", "poll_managed_bot_token", "unlink"},
				"description": "Operation to perform.",
			},
			"agent_id": map[string]any{
				"type":        "string",
				"description": "Filter list_channels by agent (agent_key or UUID). Also the target agent for poll_managed_bot_token (the new bot will be bound to this agent).",
			},
			"parent_channel_id": map[string]any{
				"type":        "string",
				"description": "Parent channel UUID. Either parent_channel_id or parent_channel_name required for request_managed_bot and poll_managed_bot_token (unless caller agent has a Telegram channel — then it defaults to that).",
			},
			"parent_channel_name": map[string]any{
				"type":        "string",
				"description": "Parent channel name (alternative to parent_channel_id).",
			},
			"operator_chat_id": map[string]any{
				"type":        "string",
				"description": "Telegram chat_id of the operator to send the deep-link message to. Required for request_managed_bot. Can be numeric ID string or @username.",
			},
			"suggested_username": map[string]any{
				"type":        "string",
				"description": "Suggested Telegram username for the new bot, e.g. ath_copywriter_ath_bot. Must end in 'bot', 5-32 chars, alphanumeric + underscore. Required for request_managed_bot and poll_managed_bot_token.",
			},
			"channel_name": map[string]any{
				"type":        "string",
				"description": "Optional override for the new channel record's name on poll_managed_bot_token. Defaults to telegram/{suggested_username}.",
			},
			"channel_id": map[string]any{
				"type":        "string",
				"description": "Channel UUID to unlink.",
			},
		},
	}
}

func (t *AgentTelegramTool) Execute(ctx context.Context, args map[string]any) *Result {
	if t.channels == nil {
		return ErrorResult("agent_telegram: channel store not wired")
	}
	if t.agents == nil {
		return ErrorResult("agent_telegram: agent store not wired")
	}
	action, _ := args["action"].(string)
	if action == "" {
		return ErrorResult("action parameter is required")
	}
	switch action {
	case "list_channels":
		return t.handleListChannels(ctx, args)
	case "request_managed_bot":
		return t.handleRequestManagedBot(ctx, args)
	case "poll_managed_bot_token":
		return t.handlePollManagedBotToken(ctx, args)
	case "unlink":
		return t.handleUnlink(ctx, args)
	default:
		return ErrorResult(fmt.Sprintf("unknown action %q. Valid: list_channels, request_managed_bot, poll_managed_bot_token, unlink", action))
	}
}

// --- list_channels ---

func (t *AgentTelegramTool) handleListChannels(ctx context.Context, args map[string]any) *Result {
	all, err := t.channels.ListAll(ctx)
	if err != nil {
		return ErrorResult(fmt.Sprintf("list failed: %v", err))
	}
	var filterAgentID *uuid.UUID
	if v, _ := args["agent_id"].(string); v != "" {
		ag, err := t.resolveAgent(ctx, v)
		if err != nil {
			return ErrorResult(err.Error())
		}
		filterAgentID = &ag.ID
	}

	out := make([]map[string]any, 0)
	for _, ch := range all {
		if ch.ChannelType != "telegram" {
			continue
		}
		if filterAgentID != nil && ch.AgentID != *filterAgentID {
			continue
		}
		out = append(out, map[string]any{
			"channel_id":   ch.ID.String(),
			"name":         ch.Name,
			"display_name": ch.DisplayName,
			"agent_id":     ch.AgentID.String(),
			"enabled":      ch.Enabled,
		})
	}
	resp := map[string]any{
		"count":    len(out),
		"channels": out,
	}
	body, _ := json.MarshalIndent(resp, "", "  ")
	return NewResult(string(body))
}

// --- request_managed_bot ---

func (t *AgentTelegramTool) handleRequestManagedBot(ctx context.Context, args map[string]any) *Result {
	suggested, _ := args["suggested_username"].(string)
	if err := validateTelegramBotUsername(suggested); err != nil {
		return ErrorResult(err.Error())
	}
	operatorChatID, _ := args["operator_chat_id"].(string)
	if operatorChatID == "" {
		return ErrorResult("operator_chat_id is required for request_managed_bot")
	}

	parent, err := t.resolveParent(ctx, args)
	if err != nil {
		return ErrorResult(err.Error())
	}
	creds, err := parseTelegramCreds(parent.Credentials)
	if err != nil {
		return ErrorResult(err.Error())
	}

	// Resolve parent bot username for the deep-link URL.
	parentUsername, err := t.tgGetBotUsername(ctx, creds.Token)
	if err != nil {
		return ErrorResult(fmt.Sprintf("getMe on parent bot failed: %v", err))
	}

	deepLink := fmt.Sprintf("https://t.me/newbot/%s/%s", parentUsername, suggested)
	text := fmt.Sprintf("Tap to create the bot @%s. Telegram will ask you to confirm.\n\n%s",
		suggested, deepLink)

	// sendMessage with an inline keyboard URL button so it renders as a
	// tap-friendly action.
	body := map[string]any{
		"chat_id": operatorChatID,
		"text":    text,
		"reply_markup": map[string]any{
			"inline_keyboard": [][]map[string]any{{
				{"text": fmt.Sprintf("Create @%s", suggested), "url": deepLink},
			}},
		},
	}
	if err := t.tgPostJSON(ctx, creds.Token, "sendMessage", body, nil); err != nil {
		return ErrorResult(fmt.Sprintf("sendMessage to operator failed: %v", err))
	}

	resp := map[string]any{
		"action":             "request_managed_bot",
		"parent_channel":     parent.Name,
		"parent_username":    parentUsername,
		"suggested_username": suggested,
		"deep_link":          deepLink,
		"operator_chat_id":   operatorChatID,
		"sent":               true,
		"next_step":          "operator taps the link in Telegram; then call poll_managed_bot_token.",
	}
	out, _ := json.MarshalIndent(resp, "", "  ")
	return NewResult(string(out))
}

// --- poll_managed_bot_token ---

func (t *AgentTelegramTool) handlePollManagedBotToken(ctx context.Context, args map[string]any) *Result {
	suggested, _ := args["suggested_username"].(string)
	if err := validateTelegramBotUsername(suggested); err != nil {
		return ErrorResult(err.Error())
	}
	targetAgentRef, _ := args["agent_id"].(string)
	if targetAgentRef == "" {
		return ErrorResult("agent_id (target agent) is required for poll_managed_bot_token")
	}
	target, err := t.resolveAgent(ctx, targetAgentRef)
	if err != nil {
		return ErrorResult(err.Error())
	}

	parent, err := t.resolveParent(ctx, args)
	if err != nil {
		return ErrorResult(err.Error())
	}
	creds, err := parseTelegramCreds(parent.Credentials)
	if err != nil {
		return ErrorResult(err.Error())
	}

	// Call getManagedBotToken on parent bot. Expected Telegram response shape:
	//   { ok: true, result: { token: "..." } }            on success
	//   { ok: false, error_code: 400, description: "..." } while bot not yet created
	var result struct {
		Token string `json:"token"`
	}
	body := map[string]any{"username": suggested}
	apiErr := t.tgPostJSON(ctx, creds.Token, "getManagedBotToken", body, &result)
	if apiErr != nil {
		// Pending state: surface as "not yet" non-error response so forge
		// can decide retry cadence. Telegram returns 400 with a description
		// like "Bad Request: managed bot not yet created" until ready.
		resp := map[string]any{
			"action":             "poll_managed_bot_token",
			"suggested_username": suggested,
			"ready":              false,
			"status":             "pending",
			"detail":             apiErr.Error(),
			"next_step":          "operator has not tapped the deep link yet, or Telegram has not finalized creation. Retry shortly.",
		}
		out, _ := json.MarshalIndent(resp, "", "  ")
		return NewResult(string(out))
	}
	if result.Token == "" {
		return ErrorResult("getManagedBotToken returned ok with empty token; unexpected Telegram response shape")
	}

	// Determine the new channel's name. Default: telegram/{suggested_username}.
	channelName, _ := args["channel_name"].(string)
	if channelName == "" {
		channelName = "telegram/" + suggested
	}

	// Prevent re-registration if a channel with this name already exists.
	if existing, _ := t.channels.GetByName(ctx, channelName); existing != nil {
		return ErrorResult(fmt.Sprintf("channel %q already exists; pass a different channel_name or call unlink first", channelName))
	}

	credsJSON, _ := json.Marshal(map[string]string{"token": result.Token})
	cfgJSON := json.RawMessage(`{}`)
	inst := &store.ChannelInstanceData{
		TenantID:    store.TenantIDFromContext(ctx),
		Name:        channelName,
		DisplayName: suggested,
		ChannelType: "telegram",
		AgentID:     target.ID,
		Credentials: credsJSON,
		Config:      cfgJSON,
		Enabled:     true,
		CreatedBy:   store.UserIDFromContext(ctx),
	}
	if err := t.channels.Create(ctx, inst); err != nil {
		return ErrorResult(fmt.Sprintf("channel create failed: %v", err))
	}

	t.emitCacheInvalidate()
	t.emitAudit(ctx, "channel_instance.created", "channel_instance", inst.ID.String())

	resp := map[string]any{
		"action":             "poll_managed_bot_token",
		"suggested_username": suggested,
		"ready":              true,
		"channel_id":         inst.ID.String(),
		"channel_name":       inst.Name,
		"agent_id":           target.AgentKey,
		"agent_uuid":         target.ID.String(),
	}
	out, _ := json.MarshalIndent(resp, "", "  ")
	return NewResult(string(out))
}

// --- unlink ---

func (t *AgentTelegramTool) handleUnlink(ctx context.Context, args map[string]any) *Result {
	var inst *store.ChannelInstanceData
	if v, _ := args["channel_id"].(string); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			return ErrorResult("channel_id must be a UUID")
		}
		ch, err := t.channels.Get(ctx, id)
		if err != nil || ch == nil {
			return ErrorResult(fmt.Sprintf("channel not found: %s", v))
		}
		inst = ch
	} else if v, _ := args["channel_name"].(string); v != "" {
		ch, err := t.channels.GetByName(ctx, v)
		if err != nil || ch == nil {
			return ErrorResult(fmt.Sprintf("channel not found by name: %s", v))
		}
		inst = ch
	} else {
		return ErrorResult("unlink requires channel_id or channel_name")
	}
	if inst.ChannelType != "telegram" {
		return ErrorResult(fmt.Sprintf("channel %q is type %q, not telegram", inst.Name, inst.ChannelType))
	}

	if err := t.channels.Delete(ctx, inst.ID); err != nil {
		return ErrorResult(fmt.Sprintf("delete failed: %v", err))
	}
	t.emitCacheInvalidate()
	t.emitAudit(ctx, "channel_instance.deleted", "channel_instance", inst.ID.String())

	resp := map[string]any{
		"action":     "unlink",
		"channel_id": inst.ID.String(),
		"name":       inst.Name,
		"deleted":    true,
	}
	out, _ := json.MarshalIndent(resp, "", "  ")
	return NewResult(string(out))
}

// --- Telegram HTTP wrappers ---

type tgEnvelope struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result,omitempty"`
	ErrorCode   int             `json:"error_code,omitempty"`
	Description string          `json:"description,omitempty"`
}

// tgPostJSON makes a JSON POST to https://api.telegram.org/bot{token}/{method}
// and decodes the envelope. If result is non-nil and Telegram returned ok,
// the result payload is decoded into it. Returns an error including the
// Telegram description when ok=false.
func (t *AgentTelegramTool) tgPostJSON(ctx context.Context, token, method string, body any, result any) error {
	if token == "" {
		return fmt.Errorf("empty bot token")
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	url := fmt.Sprintf("%s/bot%s/%s", t.apiBase, token, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.http.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	var env tgEnvelope
	if err := json.Unmarshal(respBody, &env); err != nil {
		return fmt.Errorf("decode envelope: %w (body: %s)", err, truncateForLog(string(respBody)))
	}
	if !env.OK {
		return fmt.Errorf("telegram %s: %d %s", method, env.ErrorCode, env.Description)
	}
	if result != nil && len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, result); err != nil {
			return fmt.Errorf("decode result: %w", err)
		}
	}
	return nil
}

// tgGetBotUsername calls getMe and returns the bot's username (without @).
func (t *AgentTelegramTool) tgGetBotUsername(ctx context.Context, token string) (string, error) {
	var me struct {
		Username string `json:"username"`
	}
	if err := t.tgPostJSON(ctx, token, "getMe", map[string]any{}, &me); err != nil {
		return "", err
	}
	if me.Username == "" {
		return "", fmt.Errorf("getMe returned empty username")
	}
	return me.Username, nil
}

// --- helpers ---

func (t *AgentTelegramTool) resolveAgent(ctx context.Context, ref string) (*store.AgentData, error) {
	if ref == "" {
		return nil, fmt.Errorf("agent_id is required")
	}
	if id, err := uuid.Parse(ref); err == nil {
		ag, err := t.agents.GetByID(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("agent not found: %s", ref)
		}
		return ag, nil
	}
	ag, err := t.agents.GetByKey(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("agent not found: %s", ref)
	}
	return ag, nil
}

// resolveParent resolves the parent telegram channel from args. Order of
// preference: parent_channel_id → parent_channel_name → caller agent's
// telegram channel (from AgentIDFromContext).
func (t *AgentTelegramTool) resolveParent(ctx context.Context, args map[string]any) (*store.ChannelInstanceData, error) {
	if v, _ := args["parent_channel_id"].(string); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			return nil, fmt.Errorf("parent_channel_id must be a UUID")
		}
		ch, err := t.channels.Get(ctx, id)
		if err != nil || ch == nil {
			return nil, fmt.Errorf("parent channel not found: %s", v)
		}
		if ch.ChannelType != "telegram" {
			return nil, fmt.Errorf("parent channel %q is type %q, not telegram", ch.Name, ch.ChannelType)
		}
		return ch, nil
	}
	if v, _ := args["parent_channel_name"].(string); v != "" {
		ch, err := t.channels.GetByName(ctx, v)
		if err != nil || ch == nil {
			return nil, fmt.Errorf("parent channel not found by name: %s", v)
		}
		if ch.ChannelType != "telegram" {
			return nil, fmt.Errorf("parent channel %q is type %q, not telegram", ch.Name, ch.ChannelType)
		}
		return ch, nil
	}
	// Default: any telegram channel attached to the caller agent.
	callerAgentID := store.AgentIDFromContext(ctx)
	if callerAgentID == uuid.Nil {
		return nil, fmt.Errorf("no parent channel specified and no caller agent in context; pass parent_channel_id or parent_channel_name")
	}
	all, err := t.channels.ListAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("list channels: %v", err)
	}
	for _, ch := range all {
		if ch.ChannelType == "telegram" && ch.AgentID == callerAgentID {
			cp := ch
			return &cp, nil
		}
	}
	return nil, fmt.Errorf("no telegram channel found for caller agent; pass parent_channel_id or parent_channel_name explicitly")
}

func parseTelegramCreds(raw []byte) (*struct{ Token string }, error) {
	var creds struct {
		Token string `json:"token"`
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("parent channel has empty credentials")
	}
	if err := json.Unmarshal(raw, &creds); err != nil {
		return nil, fmt.Errorf("decode parent credentials: %v", err)
	}
	if creds.Token == "" {
		return nil, fmt.Errorf("parent channel credentials have empty token")
	}
	return &struct{ Token string }{Token: creds.Token}, nil
}

// validateTelegramBotUsername applies the Bot API username rules:
// 5-32 chars, [A-Za-z0-9_], must end with "bot" (case-insensitive).
func validateTelegramBotUsername(s string) error {
	if s == "" {
		return fmt.Errorf("suggested_username is required")
	}
	if len(s) < 5 || len(s) > 32 {
		return fmt.Errorf("suggested_username must be 5-32 characters (got %d)", len(s))
	}
	for i, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_'
		if !ok {
			return fmt.Errorf("suggested_username: invalid character %q at position %d (only alphanumeric and underscore allowed)", r, i)
		}
	}
	if !strings.HasSuffix(strings.ToLower(s), "bot") {
		return fmt.Errorf("suggested_username must end with 'bot' (Telegram requirement)")
	}
	return nil
}

func (t *AgentTelegramTool) emitCacheInvalidate() {
	if t.msgBus == nil {
		return
	}
	t.msgBus.Broadcast(bus.Event{
		Name:    protocol.EventCacheInvalidate,
		Payload: bus.CacheInvalidatePayload{Kind: bus.CacheKindChannelInstances, Key: ""},
	})
}

func (t *AgentTelegramTool) emitAudit(ctx context.Context, action, entityType, entityID string) {
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

func truncateForLog(s string) string {
	const max = 200
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
