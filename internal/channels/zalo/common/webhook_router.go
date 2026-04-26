package common

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/channels"
	"github.com/nextlevelbuilder/goclaw/internal/safego"
)

// Router dispatches webhook POSTs to a registered Zalo channel instance.
// One Router is built at gateway startup and mounted on the mux at
// /channels/zalo/webhook. Channels register themselves at Start() and
// unregister at Stop() — there is no central instance lookup table on
// channels.Manager. Zalo channels deliberately do not implement
// channels.WebhookChannel because that interface mounts a per-channel
// path; we want a single-mount, multi-instance router.
type Router struct {
	mu          sync.RWMutex
	instances   map[uuid.UUID]registeredInstance
	dedup       *Dedup
	rateLimiter *channels.WebhookRateLimiter
	maxBodySize int64
}

type registeredInstance struct {
	handler  WebhookHandler
	tenantID uuid.UUID
}

// WebhookHandler is the per-channel-instance contract the router invokes
// after rate limit / signature / dedup checks pass. The handler decides
// what the parsed event means; the router knows nothing about Zalo
// payload shapes.
type WebhookHandler interface {
	HandleWebhookEvent(ctx context.Context, raw json.RawMessage) error
	SignatureVerifier() SignatureVerifier
	MessageIDExtractor() MessageIDExtractor
}

// SignatureVerifier validates per-request authenticity. Bot uses a
// header-token compare; OA uses HMAC-SHA256 over the body. Both are
// expected to use crypto/subtle.ConstantTimeCompare under the hood.
type SignatureVerifier interface {
	Verify(headers http.Header, body []byte) error
}

// MessageIDExtractor pulls the per-event id out of the raw body for
// dedup. Returning "" means the router will not dedup this event.
type MessageIDExtractor interface {
	ExtractMessageID(raw json.RawMessage) string
}

// ErrSignatureMismatch is the canonical signal a verifier returns when
// the request signature does not match. The router maps it to 401.
var ErrSignatureMismatch = errors.New("zalo_common: webhook signature mismatch")

const (
	defaultDedupTTL     = 5 * time.Minute
	defaultDedupMax     = 1000
	defaultMaxBodyBytes = 1 * 1024 * 1024
)

// NewRouter returns a router with default dedup and rate-limit
// parameters. Tests construct their own to keep state isolated (no
// process-wide singleton).
func NewRouter() *Router {
	return &Router{
		instances:   make(map[uuid.UUID]registeredInstance),
		dedup:       NewDedup(defaultDedupTTL, defaultDedupMax),
		rateLimiter: channels.NewWebhookRateLimiter(),
		maxBodySize: defaultMaxBodyBytes,
	}
}

// RegisterInstance enrolls a channel for routing. tenantID is captured
// at register time for defense-in-depth scoping in downstream handlers.
func (r *Router) RegisterInstance(id uuid.UUID, h WebhookHandler, tenantID uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.instances[id] = registeredInstance{handler: h, tenantID: tenantID}
}

// UnregisterInstance removes a channel from the routing table. Channel
// Stop() must call this to avoid leaking entries across restarts.
func (r *Router) UnregisterInstance(id uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.instances, id)
}

func (r *Router) lookup(id uuid.UUID) (registeredInstance, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	inst, ok := r.instances[id]
	return inst, ok
}

// ServeHTTP is the wire entry point. It always returns 200 once dispatch
// reaches the handler — Zalo retries hard on non-2xx, so handler errors
// are logged but not surfaced as HTTP failures. Pre-dispatch failures
// (auth, parse, rate limit) are surfaced as 4xx so operators can see
// real configuration problems.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	instanceStr := req.URL.Query().Get("instance")
	instanceID, err := uuid.Parse(instanceStr)
	if err != nil {
		http.Error(w, "bad instance", http.StatusBadRequest)
		return
	}

	if !r.rateLimiter.Allow(instanceID.String()) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}

	inst, ok := r.lookup(instanceID)
	if !ok {
		http.Error(w, "unknown instance", http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(io.LimitReader(req.Body, r.maxBodySize))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	if err := inst.handler.SignatureVerifier().Verify(req.Header, body); err != nil {
		slog.Warn("security.zalo_webhook_signature_mismatch",
			"instance_id", instanceID,
			"remote", req.RemoteAddr,
			"err", err)
		http.Error(w, "signature mismatch", http.StatusUnauthorized)
		return
	}

	if mid := inst.handler.MessageIDExtractor().ExtractMessageID(body); mid != "" {
		if r.dedup.SeenOrAdd(instanceID, mid) {
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	go r.dispatch(instanceID, inst, body)
	w.WriteHeader(http.StatusOK)
}

// dispatch invokes the handler in a goroutine so the HTTP response is
// not blocked by per-event work (Zalo expects ack within ~2s). Panics
// inside the handler are caught by safego.Recover and logged.
func (r *Router) dispatch(instanceID uuid.UUID, inst registeredInstance, body []byte) {
	defer safego.Recover(nil, "instance_id", instanceID, "tenant_id", inst.tenantID)
	ctx := context.Background()
	if err := inst.handler.HandleWebhookEvent(ctx, body); err != nil {
		slog.Error("zalo_webhook.handler_error",
			"instance_id", instanceID,
			"tenant_id", inst.tenantID,
			"err", err)
	}
}
