package bot

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/channels"
	"github.com/nextlevelbuilder/goclaw/internal/channels/zalo/common"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

const (
	defaultPollTimeout  = 30
	pollErrorBackoff    = 5 * time.Second
	pollTimeoutHeadroom = 7 * time.Second
)

func (c *Channel) pollLoop(ctx context.Context) {
	slog.Info("zalo polling loop started")

	for {
		select {
		case <-ctx.Done():
			slog.Info("zalo polling loop stopped (context)")
			return
		case <-c.stopCh:
			slog.Info("zalo polling loop stopped")
			return
		default:
		}

		updates, err := c.getUpdates(defaultPollTimeout)
		if err != nil {
			// 408 = no updates (timeout), not an error
			if !strings.Contains(err.Error(), "408") {
				slog.Warn("zalo getUpdates error", "error", err)
				select {
				case <-ctx.Done():
					return
				case <-c.stopCh:
					return
				case <-time.After(pollErrorBackoff):
				}
			}
			continue
		}

		for _, update := range updates {
			c.processUpdate(update)
		}
	}
}

func (c *Channel) processUpdate(update zaloUpdate) {
	switch update.EventName {
	case "message.text.received":
		if update.Message != nil {
			c.handleTextMessage(update.Message)
		}
	case "message.image.received":
		if update.Message != nil {
			c.handleImageMessage(update.Message)
		}
	default:
		slog.Debug("zalo unsupported event", "event", update.EventName)
	}
}

func (c *Channel) handleTextMessage(msg *zaloMessage) {
	ctx := context.Background()
	ctx = store.WithTenantID(ctx, c.TenantID())
	senderID := msg.From.ID
	if senderID == "" {
		slog.Warn("zalo: dropping text message with empty sender ID", "message_id", msg.MessageID)
		return
	}
	chatID := msg.Chat.ID
	if chatID == "" {
		chatID = senderID
	}

	// DM policy enforcement (Zalo is DM-only)
	if !c.checkDMPolicy(ctx, senderID, chatID) {
		return
	}

	content := msg.Text
	if content == "" {
		content = "[empty message]"
	}

	slog.Debug("zalo text message received",
		"sender_id", senderID,
		"chat_id", chatID,
		"preview", channels.Truncate(content, 50),
	)

	metadata := common.InboundMeta{
		MessageID:         msg.MessageID,
		Platform:          common.PlatformZaloBot,
		SenderDisplayName: msg.From.Username,
	}.ToMap()

	c.HandleMessage(senderID, chatID, content, nil, metadata, "direct")
}

func (c *Channel) handleImageMessage(msg *zaloMessage) {
	ctx := context.Background()
	ctx = store.WithTenantID(ctx, c.TenantID())
	senderID := msg.From.ID
	if senderID == "" {
		slog.Warn("zalo: dropping image message with empty sender ID", "message_id", msg.MessageID)
		return
	}
	chatID := msg.Chat.ID
	if chatID == "" {
		chatID = senderID
	}

	if !c.checkDMPolicy(ctx, senderID, chatID) {
		return
	}

	content := msg.Caption
	if content == "" {
		content = "[image]"
	}

	// Download photo from Zalo CDN to local temp file (CDN URLs are auth-restricted/expiring)
	var media []string
	var photoURL string
	switch {
	case msg.PhotoURL != "":
		photoURL = msg.PhotoURL
	case msg.Photo != "":
		photoURL = msg.Photo
	}

	if photoURL != "" {
		localPath, err := c.downloadMedia(photoURL)
		if err != nil {
			slog.Warn("zalo photo download failed, passing URL as fallback",
				"photo_url", photoURL, "error", err)
			media = []string{photoURL}
		} else {
			media = []string{localPath}
		}
	}

	slog.Info("zalo image message received",
		"sender_id", senderID,
		"chat_id", chatID,
		"photo_url", photoURL,
		"has_media", len(media) > 0,
	)

	metadata := common.InboundMeta{
		MessageID:         msg.MessageID,
		Platform:          common.PlatformZaloBot,
		SenderDisplayName: msg.From.Username,
	}.ToMap()

	c.HandleMessage(senderID, chatID, content, media, metadata, "direct")
}
