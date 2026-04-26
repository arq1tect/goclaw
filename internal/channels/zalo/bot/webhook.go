package bot

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/nextlevelbuilder/goclaw/internal/channels/zalo/common"
)

// HandleWebhookEvent decodes a single update pushed by Zalo Bot API and
// runs it through the same processUpdate path used by the long-polling
// transport. The webhook payload shape matches getUpdates.
func (c *Channel) HandleWebhookEvent(_ context.Context, raw json.RawMessage) error {
	var u zaloUpdate
	if err := json.Unmarshal(raw, &u); err != nil {
		return fmt.Errorf("zalo_bot.webhook: decode update: %w", err)
	}

	// A8: drop self-echoes. Zalo's webhook delivers our own outbound
	// sendMessage/sendPhoto results back through the same URL, which
	// would cause the bot to reply to itself in a loop. processUpdate
	// has no notion of "from me"; filter here.
	if u.Message != nil && u.Message.From.ID != "" && u.Message.From.ID == c.botID {
		slog.Debug("zalo_bot.webhook.self_echo_filtered",
			"bot_id", c.botID, "message_id", u.Message.MessageID)
		return nil
	}

	c.processUpdate(u)
	return nil
}

// SignatureVerifier returns a header-token verifier bound to the
// channel's webhook_secret. Returns the same instance every call —
// stateless, safe to share across requests.
func (c *Channel) SignatureVerifier() common.SignatureVerifier {
	return botSignatureVerifier{secret: c.webhookSecret}
}

// MessageIDExtractor pulls the per-message id out of the raw payload so
// the router can dedup before dispatch. Empty id means dedup is skipped.
func (c *Channel) MessageIDExtractor() common.MessageIDExtractor {
	return botMessageIDExtractor{}
}

// botSignatureVerifier compares X-Bot-Api-Secret-Token against the
// configured secret in constant time.
//
// B6: an empty secret is rejected up front. crypto/subtle.ConstantTimeCompare
// returns 1 when both inputs are empty, so without this guard an unset
// secret would accept every request. Start() also rejects transport=webhook
// when the secret is unset, but verify guards against config racing.
type botSignatureVerifier struct {
	secret string
}

func (v botSignatureVerifier) Verify(h http.Header, _ []byte) error {
	if v.secret == "" {
		return errors.New("zalo_bot.webhook: secret unset")
	}
	got := h.Get("X-Bot-Api-Secret-Token")
	if got == "" {
		return errors.New("zalo_bot.webhook: missing X-Bot-Api-Secret-Token")
	}
	if subtle.ConstantTimeCompare([]byte(got), []byte(v.secret)) != 1 {
		return common.ErrSignatureMismatch
	}
	return nil
}

// botMessageIDExtractor reads update.message.message_id without decoding
// the rest of the payload.
type botMessageIDExtractor struct{}

func (botMessageIDExtractor) ExtractMessageID(raw json.RawMessage) string {
	var probe struct {
		Message struct {
			MessageID string `json:"message_id"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return ""
	}
	return probe.Message.MessageID
}
