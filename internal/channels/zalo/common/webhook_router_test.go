package common

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

type fakeHandler struct {
	mu          sync.Mutex
	dispatched  atomic.Int32
	lastBody    json.RawMessage
	verifyErr   error
	extractedID string
	handlerErr  error
	panicMsg    string
	doneCh      chan struct{}
}

func newFakeHandler() *fakeHandler {
	return &fakeHandler{doneCh: make(chan struct{}, 16)}
}

func (f *fakeHandler) HandleWebhookEvent(_ context.Context, raw json.RawMessage) error {
	f.mu.Lock()
	f.lastBody = raw
	f.mu.Unlock()
	f.dispatched.Add(1)
	defer func() { f.doneCh <- struct{}{} }()
	if f.panicMsg != "" {
		panic(f.panicMsg)
	}
	return f.handlerErr
}

func (f *fakeHandler) SignatureVerifier() SignatureVerifier   { return staticVerifier{err: f.verifyErr} }
func (f *fakeHandler) MessageIDExtractor() MessageIDExtractor { return staticExtractor{id: f.extractedID} }

type staticVerifier struct{ err error }

func (v staticVerifier) Verify(_ http.Header, _ []byte) error { return v.err }

type staticExtractor struct{ id string }

func (e staticExtractor) ExtractMessageID(_ json.RawMessage) string { return e.id }

func waitForDispatch(t *testing.T, h *fakeHandler) {
	t.Helper()
	select {
	case <-h.doneCh:
	case <-time.After(time.Second):
		t.Fatalf("handler not dispatched")
	}
}

func newTestServer(t *testing.T) (*Router, uuid.UUID, *fakeHandler, *httptest.Server) {
	t.Helper()
	r := NewRouter()
	id := uuid.New()
	h := newFakeHandler()
	r.RegisterInstance(id, h, uuid.New())
	return r, id, h, httptest.NewServer(r)
}

func postBody(srv *httptest.Server, query, body string) *http.Response {
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"?"+query, strings.NewReader(body))
	resp, _ := srv.Client().Do(req)
	return resp
}

func TestRouter_RejectsNonPOST(t *testing.T) {
	_, _, _, srv := newTestServer(t)
	defer srv.Close()
	resp, _ := srv.Client().Get(srv.URL)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

func TestRouter_RejectsBadInstance(t *testing.T) {
	_, _, _, srv := newTestServer(t)
	defer srv.Close()
	resp := postBody(srv, "instance=not-a-uuid", "{}")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestRouter_404UnknownInstance(t *testing.T) {
	_, _, _, srv := newTestServer(t)
	defer srv.Close()
	resp := postBody(srv, "instance="+uuid.NewString(), "{}")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestRouter_401OnSignatureMismatch(t *testing.T) {
	_, id, h, srv := newTestServer(t)
	defer srv.Close()
	h.verifyErr = ErrSignatureMismatch
	resp := postBody(srv, "instance="+id.String(), "{}")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	if h.dispatched.Load() != 0 {
		t.Error("handler invoked despite signature mismatch")
	}
}

func TestRouter_200OnValidEventDispatches(t *testing.T) {
	_, id, h, srv := newTestServer(t)
	defer srv.Close()
	resp := postBody(srv, "instance="+id.String(), `{"x":1}`)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	waitForDispatch(t, h)
	if h.dispatched.Load() != 1 {
		t.Errorf("dispatched = %d, want 1", h.dispatched.Load())
	}
}

func TestRouter_DedupShortCircuit(t *testing.T) {
	_, id, h, srv := newTestServer(t)
	defer srv.Close()
	h.extractedID = "evt-1"
	postBody(srv, "instance="+id.String(), `{}`)
	waitForDispatch(t, h)

	resp := postBody(srv, "instance="+id.String(), `{}`)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	// Give the goroutine a beat — it should NOT have been dispatched.
	time.Sleep(50 * time.Millisecond)
	if h.dispatched.Load() != 1 {
		t.Errorf("dispatched = %d, want 1 (deduped)", h.dispatched.Load())
	}
}

func TestRouter_PanicInHandlerRecovered(t *testing.T) {
	_, id, h, srv := newTestServer(t)
	defer srv.Close()
	h.panicMsg = "boom"
	resp := postBody(srv, "instance="+id.String(), `{}`)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	// We don't assert on doneCh here — panicMsg!="" panics before the
	// deferred channel send. Just verify the HTTP response did not crash
	// the server.
}

func TestRouter_RateLimitReturns429(t *testing.T) {
	r, id, _, srv := newTestServer(t)
	defer srv.Close()
	// Burn through the limit (30/window) — 31st request must be rejected.
	for i := 0; i < 30; i++ {
		_ = postBody(srv, "instance="+id.String(), `{}`)
	}
	resp := postBody(srv, "instance="+id.String(), `{}`)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", resp.StatusCode)
	}
	_ = r
}

func TestRouter_UnregisterRemovesInstance(t *testing.T) {
	r, id, _, srv := newTestServer(t)
	defer srv.Close()
	r.UnregisterInstance(id)
	resp := postBody(srv, "instance="+id.String(), `{}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 after unregister", resp.StatusCode)
	}
}

func TestRouter_NoSingletonPerTestIsolation(t *testing.T) {
	a := NewRouter()
	b := NewRouter()
	id := uuid.New()
	a.RegisterInstance(id, newFakeHandler(), uuid.New())
	if _, ok := b.lookup(id); ok {
		t.Error("router b should not see router a's registrations")
	}
}

// silence unused-import vigilance during incremental edits.
var _ = errors.New
