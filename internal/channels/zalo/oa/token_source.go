package oa

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// refreshMargin: refresh when the access token expires within this window.
const refreshMargin = 5 * time.Minute

// refreshHTTPTimeout bounds the HTTP roundtrip while ts.mu is held so a
// misconfigured caller ctx can't wedge concurrent send/poll/reaction
// callers. Shorter than the 15s defaultClientTimeout.
const refreshHTTPTimeout = 12 * time.Second

// tokenSource lazily refreshes the access token. ts.mu serializes refresh
// (Zalo refresh tokens are single-use). Reads of creds go through the
// atomic pointer; callers must treat the returned struct as read-only.
type tokenSource struct {
	client     *Client
	creds      atomic.Pointer[ChannelCreds]
	store      store.ChannelInstanceStore
	instanceID uuid.UUID

	mu sync.Mutex
}

// Snapshot returns a read-only pointer to the current creds.
func (ts *tokenSource) Snapshot() *ChannelCreds {
	if p := ts.creds.Load(); p != nil {
		return p
	}
	return &ChannelCreds{}
}

// ForceRefresh marks the cached token stale so the next Access() refreshes.
func (ts *tokenSource) ForceRefresh() {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	cur := ts.Snapshot()
	next := *cur
	next.ExpiresAt = time.Time{}
	next.AccessToken = ""
	ts.creds.Store(&next)
}

// Access returns a valid access token, refreshing if within refreshMargin.
func (ts *tokenSource) Access(ctx context.Context) (string, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	cur := ts.Snapshot()
	if cur.AccessToken != "" && time.Until(cur.ExpiresAt) > refreshMargin {
		return cur.AccessToken, nil
	}

	if err := ts.doRefresh(ctx); err != nil {
		return "", err
	}
	return ts.Snapshot().AccessToken, nil
}

// doRefresh performs the HTTP refresh + persistence. Holds ts.mu.
// Persist-before-commit: if Persist fails after a successful refresh we
// keep the new tokens in memory (the old refresh token is already burned)
// but DB has stale tokens — next process restart will fail to invalid_grant
// and surface re-auth, which is the safe failure mode.
func (ts *tokenSource) doRefresh(ctx context.Context) error {
	cur := ts.Snapshot()
	if cur.RefreshToken == "" {
		// Pre-authorization: distinct from a burned refresh token; do NOT
		// escalate to Failed.
		return ErrNotAuthorized
	}

	refreshCtx, cancel := context.WithTimeout(ctx, refreshHTTPTimeout)
	defer cancel()
	tok, rawErr := ts.client.RefreshToken(refreshCtx, cur.AppID, cur.SecretKey, cur.RefreshToken)
	if rawErr != nil {
		err := classifyRefreshError(rawErr)
		if errors.Is(err, ErrAuthExpired) {
			slog.Warn("zalo_oa.reauth_required", "instance_id", ts.instanceID, "oa_id", cur.OAID)
			return err
		}
		slog.Warn("zalo_oa.refresh_failed", "instance_id", ts.instanceID, "oa_id", cur.OAID, "error", err)
		return err
	}

	snapshot := *cur
	snapshot.WithTokens(tok)
	if err := Persist(ctx, ts.store, ts.instanceID, &snapshot); err != nil {
		slog.Error("zalo_oa.persist_failed", "instance_id", ts.instanceID, "oa_id", cur.OAID, "error", err)
		// Commit in memory: the new pair is the only valid one until restart.
		ts.creds.Store(&snapshot)
		return err
	}
	ts.creds.Store(&snapshot)
	slog.Info("zalo_oa.token_refreshed",
		"instance_id", ts.instanceID,
		"oa_id", snapshot.OAID,
		"new_expires_at", snapshot.ExpiresAt,
		"refresh_expires_at", snapshot.RefreshTokenExpiresAt,
	)
	return nil
}
