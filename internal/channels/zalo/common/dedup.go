package common

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// Dedup is a bounded LRU+TTL cache of seen webhook message IDs, scoped per
// channel-instance UUID. The webhook router consults it to short-circuit
// retries Zalo sends after timeouts. Polling has a different dedup
// (oa/seen_ids.go) and is unaffected by this struct.
type Dedup struct {
	mu  sync.Mutex
	ttl time.Duration
	max int
	m   map[string]time.Time // key: instanceID|messageID
}

// NewDedup returns a Dedup that expires entries after ttl and caps total
// entries at max. When the cap is exceeded the oldest entry (by add time)
// is evicted on the next SeenOrAdd call.
func NewDedup(ttl time.Duration, max int) *Dedup {
	return &Dedup{
		ttl: ttl,
		max: max,
		m:   make(map[string]time.Time),
	}
}

// SeenOrAdd records the (instanceID, messageID) pair and reports whether
// the pair was already seen within the TTL window. A missing/empty
// messageID is treated as not-seen and not recorded — the caller is
// responsible for whether to allow it through.
func (d *Dedup) SeenOrAdd(instanceID uuid.UUID, messageID string) bool {
	if messageID == "" {
		return false
	}
	key := instanceID.String() + "|" + messageID

	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now()
	if t, ok := d.m[key]; ok && now.Sub(t) < d.ttl {
		return true
	}

	d.evictExpired(now)
	if len(d.m) >= d.max {
		d.evictOldest()
	}
	d.m[key] = now
	return false
}

// Len reports the current number of tracked entries (live + not-yet-pruned
// expired). Mainly for tests/metrics.
func (d *Dedup) Len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.m)
}

func (d *Dedup) evictExpired(now time.Time) {
	for k, t := range d.m {
		if now.Sub(t) >= d.ttl {
			delete(d.m, k)
		}
	}
}

func (d *Dedup) evictOldest() {
	var oldestKey string
	var oldestTime time.Time
	first := true
	for k, t := range d.m {
		if first || t.Before(oldestTime) {
			oldestKey = k
			oldestTime = t
			first = false
		}
	}
	if !first {
		delete(d.m, oldestKey)
	}
}
