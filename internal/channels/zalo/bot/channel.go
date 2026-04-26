// Package bot implements the Zalo Bot channel (static-token variant,
// distinct from the OAuth-backed Official Account in ../oa).
// Ported from OpenClaw TS extensions/zalo/.
//
// Zalo Bot API: https://bot-api.zaloplatforms.com
// DM only (no groups), text limit 2000 chars, polling + webhook modes.
package bot

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels"
	"github.com/nextlevelbuilder/goclaw/internal/channels/zalo/common"
	"github.com/nextlevelbuilder/goclaw/internal/config"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

const (
	maxTextLength     = 2000
	defaultMediaMaxMB = 5
	pairingDebounce   = 60 * time.Second
)

// Channel connects to the Zalo OA Bot API.
type Channel struct {
	*channels.BaseChannel
	token      string
	dmPolicy   string
	mediaMaxMB int
	blockReply *bool
	stopCh     chan struct{}
	client     *http.Client
	pollClient *http.Client
	// pairingService, pairingDebounce are inherited from channels.BaseChannel.

	transport     string    // "polling" (default) | "webhook"
	webhookSecret string    // guards X-Bot-Api-Secret-Token in webhook mode
	botID         string    // captured from getMe at Start; A8 self-echo filter
	instanceID    uuid.UUID // injected via SetInstanceID after construction

	// webhookRouter is wired by FactoryWithRouter; nil for the legacy
	// single-tenant config path. Used to register/unregister this instance
	// when transport == "webhook".
	webhookRouter *common.Router

	// legacyPhotoSentinelWarn fires once if any caller still emits the
	// deprecated [photo:URL] sentinel after the Media[] migration.
	legacyPhotoSentinelWarn sync.Once
}

// SetInstanceID is called by InstanceLoader after construction so the
// channel can register itself with the shared webhook router under its
// per-row UUID.
func (c *Channel) SetInstanceID(id uuid.UUID) { c.instanceID = id }

// New creates a new Zalo channel.
func New(cfg config.ZaloConfig, msgBus *bus.MessageBus, pairingSvc store.PairingStore) (*Channel, error) {
	if cfg.Token == "" {
		return nil, fmt.Errorf("zalo token is required")
	}

	base := channels.NewBaseChannel("zalo", msgBus, cfg.AllowFrom)
	base.ValidatePolicy(cfg.DMPolicy, "")

	dmPolicy := cfg.DMPolicy
	if dmPolicy == "" {
		dmPolicy = "pairing" // TS default
	}

	mediaMax := cfg.MediaMaxMB
	if mediaMax <= 0 {
		mediaMax = defaultMediaMaxMB
	}

	transport := cfg.Transport
	if transport == "" {
		transport = "polling"
	}

	ch := &Channel{
		BaseChannel:   base,
		token:         cfg.Token,
		dmPolicy:      dmPolicy,
		mediaMaxMB:    mediaMax,
		blockReply:    cfg.BlockReply,
		stopCh:        make(chan struct{}),
		client:        &http.Client{Timeout: 60 * time.Second},
		pollClient:    &http.Client{Timeout: 0},
		transport:     transport,
		webhookSecret: cfg.WebhookSecret,
	}
	ch.SetPairingService(pairingSvc)
	return ch, nil
}

// BlockReplyEnabled returns the per-channel block_reply override (nil = inherit gateway default).
func (c *Channel) BlockReplyEnabled() *bool { return c.blockReply }

// Start begins listening for Zalo updates. Behavior depends on transport:
//
//	"polling" (default): launch the long-poll loop against getUpdates.
//	"webhook":           register with the shared common.Router so Zalo's
//	                     POST /channels/zalo/webhook?instance=<uuid>
//	                     dispatches into HandleWebhookEvent. The poll loop
//	                     never starts.
func (c *Channel) Start(ctx context.Context) error {
	info, err := c.getMe()
	if err != nil {
		return fmt.Errorf("zalo getMe failed: %w", err)
	}
	c.botID = info.ID
	slog.Info("zalo bot connected",
		"bot_id", info.ID, "bot_name", info.Name, "transport", c.transport)

	c.SetRunning(true)

	switch c.transport {
	case "webhook":
		if c.webhookSecret == "" {
			c.SetRunning(false)
			return fmt.Errorf("zalo_bot: transport=webhook requires webhook_secret")
		}
		if c.webhookRouter == nil {
			c.SetRunning(false)
			return fmt.Errorf("zalo_bot: transport=webhook requires shared router (use FactoryWithRouter)")
		}
		c.webhookRouter.RegisterInstance(c.instanceID, c, c.TenantID())
		slog.Info("zalo_bot.webhook.registered",
			"instance_id", c.instanceID, "bot_id", c.botID)
	case "polling":
		go c.pollLoop(ctx)
	default:
		c.SetRunning(false)
		return fmt.Errorf("zalo_bot: unknown transport %q", c.transport)
	}
	return nil
}

// Stop shuts down the Zalo bot. Webhook mode unregisters from the shared
// router so subsequent requests get a clean 404 instead of dispatching to
// a stopped channel.
func (c *Channel) Stop(_ context.Context) error {
	slog.Info("stopping zalo bot", "transport", c.transport)
	if c.transport == "webhook" && c.webhookRouter != nil {
		c.webhookRouter.UnregisterInstance(c.instanceID)
	}
	close(c.stopCh)
	c.SetRunning(false)
	return nil
}

// Send delivers an outbound message to a Zalo chat.
func (c *Channel) Send(_ context.Context, msg bus.OutboundMessage) error {
	if !c.IsRunning() {
		return fmt.Errorf("zalo bot not running")
	}

	// Strip markdown — Zalo does not support any markup rendering.
	msg.Content = StripMarkdown(msg.Content)

	// Defensive: warn if any caller still emits the legacy [photo:URL] sentinel
	// after the migration. Logged once per process to avoid log spam.
	if strings.Contains(msg.Content, "[photo:") {
		c.legacyPhotoSentinelWarn.Do(func() {
			slog.Warn("zalo_bot.send.legacy_photo_sentinel_detected",
				"chat_id", msg.ChatID,
				"hint", "switch caller to bus.OutboundMessage.Media[]")
		})
	}

	if len(msg.Media) == 0 {
		return c.sendChunkedText(msg.ChatID, msg.Content)
	}
	if len(msg.Media) > 1 {
		slog.Info("zalo_bot.send.extra_media_skipped",
			"chat_id", msg.ChatID, "extra", len(msg.Media)-1)
	}

	m := msg.Media[0]
	if !isHTTPURL(m.URL) {
		return fmt.Errorf("zalo_bot: local file media not supported; use zalo_oa channel (got %q)", m.URL)
	}
	caption := mergeTrailingText(m.Caption, msg.Content)
	return c.sendPhoto(msg.ChatID, m.URL, caption)
}

