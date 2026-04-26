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

	// webhookRouter is the shared Zalo router for the gateway. Wired by
	// FactoryWithRouter; nil for callers that still use the legacy Factory
	// (e.g. legacy single-tenant config path). Phase 04 calls
	// router.RegisterInstance(...) when transport=webhook.
	webhookRouter *common.Router

	// legacyPhotoSentinelWarn fires once-per-process if any caller still
	// emits the deprecated [photo:URL] sentinel after the Media[] migration.
	legacyPhotoSentinelWarn sync.Once
}

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

	ch := &Channel{
		BaseChannel: base,
		token:       cfg.Token,
		dmPolicy:    dmPolicy,
		mediaMaxMB:  mediaMax,
		blockReply:  cfg.BlockReply,
		stopCh:      make(chan struct{}),
		client:      &http.Client{Timeout: 60 * time.Second},
		pollClient:  &http.Client{Timeout: 0},
	}
	ch.SetPairingService(pairingSvc)
	return ch, nil
}

// BlockReplyEnabled returns the per-channel block_reply override (nil = inherit gateway default).
func (c *Channel) BlockReplyEnabled() *bool { return c.blockReply }

// Start begins polling for Zalo updates.
func (c *Channel) Start(ctx context.Context) error {
	slog.Info("starting zalo bot (polling mode)")

	// Validate token
	info, err := c.getMe()
	if err != nil {
		return fmt.Errorf("zalo getMe failed: %w", err)
	}
	slog.Info("zalo bot connected", "bot_id", info.ID, "bot_name", info.Name)

	c.SetRunning(true)

	go c.pollLoop(ctx)

	return nil
}

// Stop shuts down the Zalo bot.
func (c *Channel) Stop(_ context.Context) error {
	slog.Info("stopping zalo bot")
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

// isHTTPURL reports whether u is an http or https URL. Bot's sendPhoto API
// only accepts remote URLs; local paths must be rejected.
func isHTTPURL(u string) bool {
	return strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")
}

// mergeTrailingText joins caption + content with a blank line. Mirrors
// zalo/oa's mergeTrailingText so users see consistent layout across channels.
func mergeTrailingText(caption, content string) string {
	caption = strings.TrimSpace(caption)
	content = strings.TrimSpace(content)
	switch {
	case caption == "" && content == "":
		return ""
	case caption == "":
		return content
	case content == "":
		return caption
	default:
		return caption + "\n\n" + content
	}
}

