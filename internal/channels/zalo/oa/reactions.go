package oa

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

const reactionDebounceMs = 700 * time.Millisecond

// Tone tuned for OA's B2C surface: one "received, working" ack on the
// first intermediate event plus a warm/sad terminal. tool/coding/web are
// intentionally NOT mapped — chatty mid-run reactions look unprofessional
// in customer chats and burn through the 50-per-message cap.
//
// Angry (`:-h`) is intentionally excluded — dropping an angry face on the
// customer's own message reads as blaming them, even on agent-side errors.
var statusReactionVariants = map[string][]string{
	"thinking": {reactionIconLike, reactionIconHeart},
	"done":     {reactionIconHeart, reactionIconLike},
	"error":    {reactionIconWorry, reactionIconCry},
}

func resolveReactionEmoji(status string) string {
	variants, ok := statusReactionVariants[status]
	if !ok {
		return ""
	}
	for _, v := range variants {
		if zaloSupportedReactions[v] {
			return v
		}
	}
	return ""
}

type zaloReactionController struct {
	ch              *Channel
	userID          string
	sourceMessageID string

	mu            sync.Mutex
	currentIcon   string
	lastStatus    string
	terminal      bool
	debounceTimer *time.Timer
}

func newZaloReactionController(ch *Channel, userID, sourceMessageID string) *zaloReactionController {
	return &zaloReactionController{
		ch:              ch,
		userID:          userID,
		sourceMessageID: sourceMessageID,
	}
}

func (rc *zaloReactionController) SetStatus(ctx context.Context, status string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	if rc.terminal {
		return
	}
	rc.lastStatus = status

	if status == "done" || status == "error" {
		rc.terminal = true
		rc.cancelDebounceLocked()
		if icon := resolveReactionEmoji(status); icon != "" {
			rc.applyReactionLocked(ctx, icon)
		}
		return
	}

	if _, mapped := statusReactionVariants[status]; !mapped {
		return
	}

	rc.cancelDebounceLocked()
	rc.debounceTimer = time.AfterFunc(reactionDebounceMs, func() {
		rc.mu.Lock()
		defer rc.mu.Unlock()
		if rc.terminal {
			return
		}
		if icon := resolveReactionEmoji(rc.lastStatus); icon != "" {
			// Original ctx is gone by timer fire; mirror Telegram's pattern.
			rc.applyReactionLocked(context.Background(), icon)
		}
	})
}

func (rc *zaloReactionController) Stop() {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.cancelDebounceLocked()
}

func (rc *zaloReactionController) cancelDebounceLocked() {
	if rc.debounceTimer != nil {
		rc.debounceTimer.Stop()
		rc.debounceTimer = nil
	}
}

// applyReactionLocked: caller MUST hold rc.mu. On error, leaves currentIcon
// unset so the next transition retries. Never flips channel health.
func (rc *zaloReactionController) applyReactionLocked(ctx context.Context, icon string) {
	if icon == rc.currentIcon {
		return
	}
	if _, err := rc.ch.SendReaction(ctx, rc.userID, rc.sourceMessageID, icon); err != nil {
		slog.Debug("zalo_oa.reaction.set_failed",
			"user_id", rc.userID,
			"source_message_id", rc.sourceMessageID,
			"icon", icon,
			"error", err)
		return
	}
	rc.currentIcon = icon
}

// chatID for Zalo OA is the user_id (1:1 DM), so it doubles as recipient.
func (c *Channel) OnReactionEvent(ctx context.Context, chatID, messageID, status string) error {
	if c.cfg.ReactionLevel == "" || c.cfg.ReactionLevel == "off" {
		return nil
	}
	if c.cfg.ReactionLevel == "minimal" && status != "done" && status != "error" {
		return nil
	}
	if chatID == "" || messageID == "" {
		return nil
	}

	key := chatID + ":" + messageID
	val, ok := c.reactions.Load(key)
	if !ok {
		val, _ = c.reactions.LoadOrStore(key, newZaloReactionController(c, chatID, messageID))
	}
	rc, ok := val.(*zaloReactionController)
	if !ok {
		return nil
	}
	rc.SetStatus(ctx, status)

	if status == "done" || status == "error" {
		c.reactions.Delete(key)
	}
	return nil
}

func (c *Channel) ClearReaction(ctx context.Context, chatID, messageID string) error {
	if chatID == "" || messageID == "" {
		return nil
	}
	key := chatID + ":" + messageID
	if val, ok := c.reactions.LoadAndDelete(key); ok {
		if rc, ok := val.(*zaloReactionController); ok {
			rc.Stop()
		}
	}
	return c.SendClearReaction(ctx, chatID, messageID)
}
