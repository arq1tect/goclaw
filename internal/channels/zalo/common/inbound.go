package common

// Platform values written into inbound message metadata. Downstream
// consumers (logging, analytics, agent prompts) discriminate channel
// flavor by this string.
//
// Note: PlatformZaloBot is "zalo_bot", not "zalo" — bot's pre-unification
// metadata used "zalo". This is a silent breaking change for any consumer
// keyed on the literal "zalo" value (S1 in the plan). The migration was
// audited via repo-wide grep before the rename landed.
const (
	PlatformZaloBot = "zalo_bot"
	PlatformZaloOA  = "zalo_oa"
)

// InboundMeta captures the channel-agnostic per-message metadata that
// both bot and oa publish to the message bus. It exists to keep the
// metadata-map shape consistent across channel flavors.
type InboundMeta struct {
	MessageID         string
	Platform          string // PlatformZaloBot or PlatformZaloOA
	SenderDisplayName string // optional
}

// ToMap returns the metadata-map shape expected by BaseChannel.HandleMessage.
// Empty optional fields are omitted.
func (m InboundMeta) ToMap() map[string]string {
	out := map[string]string{
		"platform": m.Platform,
	}
	if m.MessageID != "" {
		out["message_id"] = m.MessageID
	}
	if m.SenderDisplayName != "" {
		out["sender_display_name"] = m.SenderDisplayName
	}
	return out
}
