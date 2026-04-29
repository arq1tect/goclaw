package oa

import (
	"context"
	"log/slog"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/channels"
)

// startWebhookTransport registers this channel with the shared router and
// optionally fires the catch-up sweep. Returns nil even on misconfig — the
// channel marks itself Failed so the dashboard surfaces the error rather
// than crashing instance_loader. Called from Channel.Start when
// cfg.Transport == "webhook".
func (c *Channel) startWebhookTransport() error {
	mode := normalizeMode(c.cfg.WebhookSignatureMode)
	if c.cfg.WebhookOASecretKey == "" && mode != SignatureModeDisabled {
		c.MarkFailed("webhook secret missing",
			"transport=webhook with signature_mode=strict|log_only requires webhook_oa_secret_key",
			channels.ChannelFailureKindConfig, false)
		return nil
	}
	c.webhookRouter.RegisterInstance(c.instanceID, c, c.TenantID())
	slog.Info("zalo_oa.webhook.registered",
		"instance_id", c.instanceID, "oa_id", c.creds.OAID, "signature_mode", mode)

	if c.cfg.CatchUpOnRestart {
		// B4: spawn in goroutine so Start returns immediately and doesn't
		// trip instance_loader.startChannelWithTimeout.
		// N2: track in WaitGroup + cancel ctx on stopCh so Stop() drains
		// cleanly without leaking.
		c.catchUpWG.Add(1)
		go c.runCatchUpSweepGoroutine()
	}
	c.MarkHealthy("webhook")
	return nil
}

// runCatchUpSweepGoroutine wraps runCatchUpSweep with WaitGroup tracking
// and stop-channel-aware cancellation so Stop() can wait for it to drain.
func (c *Channel) runCatchUpSweepGoroutine() {
	defer c.catchUpWG.Done()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Honor Stop signal — closing stopCh cancels the sweep ctx so an
	// in-flight listrecentchat call exits promptly.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-c.stopCh:
			cancel()
		case <-done:
		}
	}()
	c.runCatchUpSweep(ctx)
}
