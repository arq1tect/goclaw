package common

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/channels"
	"github.com/nextlevelbuilder/goclaw/internal/safego"
)

// Router dispatches webhook POSTs to a registered Zalo channel instance.
// A process-global Router (see shared.go) is mounted on the mux at
// WebhookPath via the generic channels.WebhookChannel iteration; both
// bot.Channel and oa.Channel implement WebhookChannel and call
// SharedRouter().MountRoute() — the routeHandled flag in MountRoute
// guarantees a single mount across both channel families. Channels
// register themselves per-instance at Start() and unregister at Stop().
type Router struct {
	mu          sync.RWMutex
	instances   map[uuid.UUID]*registeredInstance
	dedup       *Dedup
	rateLimiter *channels.WebhookRateLimiter
	maxBodySize int64

	// routeMu guards routeHandled. Separate from `mu` (which guards the
	// hot-path instance map) because MountRoute is called once per channel
	// at boot — no need to contend with ServeHTTP's RLock pattern.
	routeMu      sync.Mutex
	routeHandled bool
}

// MountRoute returns (WebhookPath, r) on the first call and ("", nil) on
// every subsequent call. Pattern mirrors facebook/webhook_router.go and
// pancake/webhook_handler.go. The routeHandled flag is sticky across
// instance_loader.Reload — http.ServeMux retains the route across the
// instance lifecycle, so re-mounting would panic with "multiple
// registrations".
func (r *Router) MountRoute() (string, http.Handler) {
	r.routeMu.Lock()
	defer r.routeMu.Unlock()
	if !r.routeHandled {
		r.routeHandled = true
		return WebhookPath, r
	}
	return "", nil
}

// emptyIDStreakWarnThreshold is the consecutive count of empty
// ExtractMessageID() returns that triggers a single warn-level log. R3-2:
// catches Zalo schema drift where the extractor silently disables dedup.
const emptyIDStreakWarnThreshold = 10

type registeredInstance struct {
	handler  WebhookHandler
	tenantID uuid.UUID

	// ctx is the per-instance dispatch context; cancelled in
	// UnregisterInstance so in-flight HandleWebhookEvent goroutines bail
	// promptly during channel Stop (R3-3).
	ctx    context.Context
	cancel context.CancelFunc

	// emptyIDStreak counts consecutive empty ExtractMessageID() returns.
	// Reset on any non-empty extraction. Warn fires once per threshold
	// crossing — see emptyIDStreakWarnThreshold (R3-2).
	emptyIDStreak atomic.Int64
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
		instances:   make(map[uuid.UUID]*registeredInstance),
		dedup:       NewDedup(defaultDedupTTL, defaultDedupMax),
		rateLimiter: channels.NewWebhookRateLimiter(),
		maxBodySize: defaultMaxBodyBytes,
	}
}

// RegisterInstance enrolls a channel for routing. tenantID is captured
// at register time for defense-in-depth scoping in downstream handlers.
// The per-instance ctx is cancelled when UnregisterInstance runs so any
// in-flight HandleWebhookEvent dispatch can observe cancellation (R3-3).
func (r *Router) RegisterInstance(id uuid.UUID, h WebhookHandler, tenantID uuid.UUID) {
	ctx, cancel := context.WithCancel(context.Background())
	inst := &registeredInstance{
		handler:  h,
		tenantID: tenantID,
		ctx:      ctx,
		cancel:   cancel,
	}
	r.mu.Lock()
	r.instances[id] = inst
	r.mu.Unlock()
}

// UnregisterInstance removes a channel from the routing table and
// cancels its dispatch context so in-flight handlers exit promptly.
// Idempotent — calling on an unregistered ID is a no-op.
func (r *Router) UnregisterInstance(id uuid.UUID) {
	r.mu.Lock()
	inst, ok := r.instances[id]
	delete(r.instances, id)
	r.mu.Unlock()
	if ok && inst.cancel != nil {
		inst.cancel()
	}
}

func (r *Router) lookup(id uuid.UUID) (*registeredInstance, bool) {
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

	mid := inst.handler.MessageIDExtractor().ExtractMessageID(body)
	if mid == "" {
		// R3-2: increment streak; warn-and-reset at threshold so a silent
		// schema drift (extractor returning "" for every event) doesn't go
		// unnoticed. Reset-after-warn throttles to one warn per 10-event window.
		n := inst.emptyIDStreak.Add(1)
		if n >= emptyIDStreakWarnThreshold {
			inst.emptyIDStreak.Store(0)
			slog.Warn("zalo_webhook.empty_message_id_streak",
				"count", n,
				"instance_id", instanceID,
				"hint", "extractor may need update for schema drift")
		}
	} else {
		inst.emptyIDStreak.Store(0)
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
// inside the handler are caught by safego.Recover and logged. The
// per-instance ctx is cancelled by UnregisterInstance so a long-running
// handler bails fast when the channel stops (R3-3).
func (r *Router) dispatch(instanceID uuid.UUID, inst *registeredInstance, body []byte) {
	defer safego.Recover(nil, "instance_id", instanceID, "tenant_id", inst.tenantID)
	if err := inst.handler.HandleWebhookEvent(inst.ctx, body); err != nil {
		slog.Error("zalo_webhook.handler_error",
			"instance_id", instanceID,
			"tenant_id", inst.tenantID,
			"err", err)
	}
}
