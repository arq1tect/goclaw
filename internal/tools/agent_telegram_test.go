package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// --- channel store fake ---

type fakeChannelStore struct {
	mu        sync.Mutex
	byID      map[uuid.UUID]*store.ChannelInstanceData
	byName    map[string]*store.ChannelInstanceData
	createErr error
	deleteErr error
}

func newFakeChannelStore() *fakeChannelStore {
	return &fakeChannelStore{
		byID:   make(map[uuid.UUID]*store.ChannelInstanceData),
		byName: make(map[string]*store.ChannelInstanceData),
	}
}

func (f *fakeChannelStore) addTelegramChannel(name string, agentID uuid.UUID, token string) *store.ChannelInstanceData {
	f.mu.Lock()
	defer f.mu.Unlock()
	creds, _ := json.Marshal(map[string]string{"token": token})
	inst := &store.ChannelInstanceData{
		Name:        name,
		DisplayName: name,
		ChannelType: "telegram",
		AgentID:     agentID,
		Credentials: creds,
		Config:      json.RawMessage(`{}`),
		Enabled:     true,
	}
	inst.ID = uuid.New()
	f.byID[inst.ID] = inst
	f.byName[name] = inst
	return inst
}

func (f *fakeChannelStore) Create(_ context.Context, inst *store.ChannelInstanceData) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		err := f.createErr
		f.createErr = nil
		return err
	}
	if _, ok := f.byName[inst.Name]; ok {
		return errors.New("duplicate name")
	}
	if inst.ID == uuid.Nil {
		inst.ID = uuid.New()
	}
	cp := *inst
	f.byID[inst.ID] = &cp
	f.byName[inst.Name] = &cp
	return nil
}

func (f *fakeChannelStore) Get(_ context.Context, id uuid.UUID) (*store.ChannelInstanceData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ch, ok := f.byID[id]; ok {
		cp := *ch
		return &cp, nil
	}
	return nil, errors.New("not found")
}

func (f *fakeChannelStore) GetByName(_ context.Context, name string) (*store.ChannelInstanceData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ch, ok := f.byName[name]; ok {
		cp := *ch
		return &cp, nil
	}
	return nil, errors.New("not found")
}

func (f *fakeChannelStore) Update(_ context.Context, _ uuid.UUID, _ map[string]any) error {
	return nil
}

func (f *fakeChannelStore) Delete(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		err := f.deleteErr
		f.deleteErr = nil
		return err
	}
	if ch, ok := f.byID[id]; ok {
		delete(f.byName, ch.Name)
		delete(f.byID, id)
	}
	return nil
}

func (f *fakeChannelStore) ListEnabled(_ context.Context) ([]store.ChannelInstanceData, error) {
	return f.ListAll(context.Background())
}

func (f *fakeChannelStore) ListAll(_ context.Context) ([]store.ChannelInstanceData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.ChannelInstanceData, 0, len(f.byID))
	for _, ch := range f.byID {
		out = append(out, *ch)
	}
	return out, nil
}

func (f *fakeChannelStore) ListAllInstances(_ context.Context) ([]store.ChannelInstanceData, error) {
	return f.ListAll(context.Background())
}

func (f *fakeChannelStore) ListAllEnabled(_ context.Context) ([]store.ChannelInstanceData, error) {
	return f.ListAll(context.Background())
}

func (f *fakeChannelStore) ListPaged(_ context.Context, _ store.ChannelInstanceListOpts) ([]store.ChannelInstanceData, error) {
	return f.ListAll(context.Background())
}

func (f *fakeChannelStore) CountInstances(_ context.Context, _ store.ChannelInstanceListOpts) (int, error) {
	return len(f.byID), nil
}

// --- agent store fake (reuse minimal pattern) ---

type telegramAgentFake struct {
	*stubAgentStore
	mu    sync.Mutex
	byKey map[string]*store.AgentData
	byID  map[uuid.UUID]*store.AgentData
}

func newTelegramAgentFake() *telegramAgentFake {
	return &telegramAgentFake{
		stubAgentStore: &stubAgentStore{},
		byKey:          make(map[string]*store.AgentData),
		byID:           make(map[uuid.UUID]*store.AgentData),
	}
}

func (f *telegramAgentFake) addAgent(key string) *store.AgentData {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := uuid.New()
	ag := &store.AgentData{AgentKey: key}
	ag.ID = id
	f.byKey[key] = ag
	f.byID[id] = ag
	return ag
}

func (f *telegramAgentFake) GetByKey(_ context.Context, key string) (*store.AgentData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ag, ok := f.byKey[key]; ok {
		return ag, nil
	}
	return nil, errors.New("agent not found: " + key)
}

func (f *telegramAgentFake) GetByID(_ context.Context, id uuid.UUID) (*store.AgentData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ag, ok := f.byID[id]; ok {
		return ag, nil
	}
	return nil, errors.New("agent not found")
}

// --- Telegram API fixture ---

// telegramFixture is a minimal httptest.Server that responds to Bot API
// methods relevant to agent_telegram. Each method's response can be
// customized per-test.
type telegramFixture struct {
	server         *httptest.Server
	getMeUsername  string
	getMeFail      bool
	tokenForPoll   string
	pollFail       bool
	pollDescription string
	sendMessageOK  bool
	sendMessageReq map[string]any
	requestCount   map[string]int
	mu             sync.Mutex
}

func newTelegramFixture(t *testing.T) *telegramFixture {
	t.Helper()
	f := &telegramFixture{
		getMeUsername: "parent_test_bot",
		sendMessageOK: true,
		requestCount:  make(map[string]int),
	}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Path: /bot{token}/{method}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if len(parts) != 2 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		method := parts[1]
		f.mu.Lock()
		f.requestCount[method]++
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch method {
		case "getMe":
			if f.getMeFail {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"ok": false, "error_code": 401, "description": "Unauthorized",
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true, "result": map[string]any{"username": f.getMeUsername},
			})
		case "sendMessage":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.sendMessageReq = body
			f.mu.Unlock()
			if !f.sendMessageOK {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"ok": false, "error_code": 400, "description": "Bad Request: chat not found",
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true, "result": map[string]any{"message_id": 42},
			})
		case "getManagedBotToken":
			if f.pollFail {
				desc := f.pollDescription
				if desc == "" {
					desc = "Bad Request: managed bot not yet created"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"ok": false, "error_code": 400, "description": desc,
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true, "result": map[string]any{"token": f.tokenForPoll},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": false, "error_code": 404, "description": "Method not found: " + method,
			})
		}
	}))
	t.Cleanup(func() { f.server.Close() })
	return f
}

// --- harness ---

func mkTelegramTool(t *testing.T) (*AgentTelegramTool, *telegramAgentFake, *fakeChannelStore, *telegramFixture) {
	t.Helper()
	af := newTelegramAgentFake()
	cf := newFakeChannelStore()
	fixture := newTelegramFixture(t)
	tool := NewAgentTelegramTool()
	tool.SetAgentStore(af)
	tool.SetChannelStore(cf)
	tool.SetAPIBase(fixture.server.URL)
	return tool, af, cf, fixture
}

// --- username validation ---

func TestAgentTelegram_UsernameValidation(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"", false},
		{"abc", false},        // too short
		{"abot", false},       // 4 chars
		{strings.Repeat("a", 30) + "bot", false}, // 33 chars
		{"my-bot", false},     // invalid char
		{"my_bot", true},
		{"forge_ath_bot", true},
		{"forge", false},      // no bot suffix
		{"ath_copywriter_ath_bot", true},
	}
	for _, c := range cases {
		err := validateTelegramBotUsername(c.name)
		if (err == nil) != c.ok {
			t.Errorf("validate(%q): ok=%v, err=%v", c.name, c.ok, err)
		}
	}
}

// --- dispatch ---

func TestAgentTelegram_NoStore(t *testing.T) {
	tool := NewAgentTelegramTool()
	r := tool.Execute(context.Background(), map[string]any{"action": "list_channels"})
	if !r.IsError || !strings.Contains(r.ForLLM, "store not wired") {
		t.Fatalf("expected store-not-wired error, got: %+v", r)
	}
}

func TestAgentTelegram_UnknownAction(t *testing.T) {
	tool, _, _, _ := mkTelegramTool(t)
	r := tool.Execute(context.Background(), map[string]any{"action": "spawn"})
	if !r.IsError || !strings.Contains(r.ForLLM, "unknown action") {
		t.Fatalf("expected unknown-action, got: %+v", r)
	}
}

// --- list_channels ---

func TestAgentTelegram_ListChannels_Empty(t *testing.T) {
	tool, _, _, _ := mkTelegramTool(t)
	r := tool.Execute(context.Background(), map[string]any{"action": "list_channels"})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	var resp map[string]any
	_ = json.Unmarshal([]byte(r.ForLLM), &resp)
	if resp["count"].(float64) != 0 {
		t.Errorf("expected count=0, got: %v", resp["count"])
	}
}

func TestAgentTelegram_ListChannels_FilterByAgent(t *testing.T) {
	tool, af, cf, _ := mkTelegramTool(t)
	ag1 := af.addAgent("alpha")
	ag2 := af.addAgent("beta")
	cf.addTelegramChannel("telegram/alpha", ag1.ID, "tok1")
	cf.addTelegramChannel("telegram/beta", ag2.ID, "tok2")

	r := tool.Execute(context.Background(), map[string]any{"action": "list_channels", "agent_id": "alpha"})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	var resp map[string]any
	_ = json.Unmarshal([]byte(r.ForLLM), &resp)
	if resp["count"].(float64) != 1 {
		t.Errorf("expected count=1, got: %v", resp["count"])
	}
}

// --- request_managed_bot ---

func TestAgentTelegram_RequestManagedBot_MissingUsername(t *testing.T) {
	tool, _, _, _ := mkTelegramTool(t)
	r := tool.Execute(context.Background(), map[string]any{
		"action": "request_managed_bot", "operator_chat_id": "12345",
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "suggested_username") {
		t.Fatalf("expected username error, got: %+v", r)
	}
}

func TestAgentTelegram_RequestManagedBot_MissingOperatorChatID(t *testing.T) {
	tool, _, _, _ := mkTelegramTool(t)
	r := tool.Execute(context.Background(), map[string]any{
		"action": "request_managed_bot", "suggested_username": "test_ath_bot",
		"parent_channel_name": "telegram/forge",
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "operator_chat_id") {
		t.Fatalf("expected operator_chat_id error, got: %+v", r)
	}
}

func TestAgentTelegram_RequestManagedBot_ParentNotFound(t *testing.T) {
	tool, _, _, _ := mkTelegramTool(t)
	r := tool.Execute(context.Background(), map[string]any{
		"action":              "request_managed_bot",
		"suggested_username":  "test_ath_bot",
		"operator_chat_id":    "12345",
		"parent_channel_name": "telegram/ghost",
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "parent channel not found") {
		t.Fatalf("expected parent-not-found, got: %+v", r)
	}
}

func TestAgentTelegram_RequestManagedBot_OK(t *testing.T) {
	tool, af, cf, fx := mkTelegramTool(t)
	ag := af.addAgent("forge")
	cf.addTelegramChannel("telegram/forge", ag.ID, "parent_tok")

	r := tool.Execute(context.Background(), map[string]any{
		"action":              "request_managed_bot",
		"suggested_username":  "ath_copywriter_ath_bot",
		"operator_chat_id":    "12345",
		"parent_channel_name": "telegram/forge",
	})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	var resp map[string]any
	_ = json.Unmarshal([]byte(r.ForLLM), &resp)
	if resp["sent"] != true {
		t.Errorf("expected sent=true, got: %v", resp["sent"])
	}
	expectedURL := "https://t.me/newbot/parent_test_bot/ath_copywriter_ath_bot"
	if resp["deep_link"] != expectedURL {
		t.Errorf("deep_link mismatch: %v", resp["deep_link"])
	}
	// Verify sendMessage was called with the expected URL in inline keyboard.
	body, _ := json.Marshal(fx.sendMessageReq)
	if !strings.Contains(string(body), expectedURL) {
		t.Errorf("sendMessage body missing deep link: %s", body)
	}
	if fx.requestCount["getMe"] != 1 || fx.requestCount["sendMessage"] != 1 {
		t.Errorf("unexpected request counts: %v", fx.requestCount)
	}
}

func TestAgentTelegram_RequestManagedBot_GetMeFails(t *testing.T) {
	tool, af, cf, fx := mkTelegramTool(t)
	ag := af.addAgent("forge")
	cf.addTelegramChannel("telegram/forge", ag.ID, "bad_tok")
	fx.getMeFail = true

	r := tool.Execute(context.Background(), map[string]any{
		"action":              "request_managed_bot",
		"suggested_username":  "test_ath_bot",
		"operator_chat_id":    "12345",
		"parent_channel_name": "telegram/forge",
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "getMe") {
		t.Fatalf("expected getMe error, got: %+v", r)
	}
}

// --- poll_managed_bot_token ---

func TestAgentTelegram_Poll_Pending(t *testing.T) {
	tool, af, cf, fx := mkTelegramTool(t)
	forge := af.addAgent("forge")
	target := af.addAgent("ath-copywriter")
	cf.addTelegramChannel("telegram/forge", forge.ID, "parent_tok")
	fx.pollFail = true // Telegram says not yet
	_ = target

	r := tool.Execute(context.Background(), map[string]any{
		"action":              "poll_managed_bot_token",
		"suggested_username":  "ath_copywriter_ath_bot",
		"agent_id":            "ath-copywriter",
		"parent_channel_name": "telegram/forge",
	})
	if r.IsError {
		t.Fatalf("pending should not error: %s", r.ForLLM)
	}
	var resp map[string]any
	_ = json.Unmarshal([]byte(r.ForLLM), &resp)
	if resp["ready"] != false {
		t.Errorf("expected ready=false on pending, got: %v", resp["ready"])
	}
	if resp["status"] != "pending" {
		t.Errorf("expected status=pending, got: %v", resp["status"])
	}
}

func TestAgentTelegram_Poll_Ready(t *testing.T) {
	tool, af, cf, fx := mkTelegramTool(t)
	forge := af.addAgent("forge")
	target := af.addAgent("ath-copywriter")
	cf.addTelegramChannel("telegram/forge", forge.ID, "parent_tok")
	fx.tokenForPoll = "9999999:CHILD_TOKEN_XYZ"

	r := tool.Execute(context.Background(), map[string]any{
		"action":              "poll_managed_bot_token",
		"suggested_username":  "ath_copywriter_ath_bot",
		"agent_id":            "ath-copywriter",
		"parent_channel_name": "telegram/forge",
	})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	var resp map[string]any
	_ = json.Unmarshal([]byte(r.ForLLM), &resp)
	if resp["ready"] != true {
		t.Errorf("expected ready=true, got: %v", resp["ready"])
	}
	// Verify channel was created
	ch, err := cf.GetByName(context.Background(), "telegram/ath_copywriter_ath_bot")
	if err != nil || ch == nil {
		t.Fatalf("new channel not stored")
	}
	if ch.AgentID != target.ID {
		t.Errorf("channel bound to wrong agent: %s vs %s", ch.AgentID, target.ID)
	}
	var creds map[string]string
	_ = json.Unmarshal(ch.Credentials, &creds)
	if creds["token"] != "9999999:CHILD_TOKEN_XYZ" {
		t.Errorf("token mismatch: %s", creds["token"])
	}
}

func TestAgentTelegram_Poll_DuplicateChannel(t *testing.T) {
	tool, af, cf, fx := mkTelegramTool(t)
	forge := af.addAgent("forge")
	target := af.addAgent("ath-copywriter")
	cf.addTelegramChannel("telegram/forge", forge.ID, "parent_tok")
	cf.addTelegramChannel("telegram/ath_copywriter_ath_bot", target.ID, "stale_tok")
	fx.tokenForPoll = "9999999:CHILD"

	r := tool.Execute(context.Background(), map[string]any{
		"action":              "poll_managed_bot_token",
		"suggested_username":  "ath_copywriter_ath_bot",
		"agent_id":            "ath-copywriter",
		"parent_channel_name": "telegram/forge",
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "already exists") {
		t.Fatalf("expected duplicate error, got: %+v", r)
	}
}

func TestAgentTelegram_Poll_MissingTargetAgent(t *testing.T) {
	tool, _, _, _ := mkTelegramTool(t)
	r := tool.Execute(context.Background(), map[string]any{
		"action":             "poll_managed_bot_token",
		"suggested_username": "ath_copywriter_ath_bot",
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "agent_id") {
		t.Fatalf("expected target agent error, got: %+v", r)
	}
}

// --- unlink ---

func TestAgentTelegram_Unlink_ByName(t *testing.T) {
	tool, af, cf, _ := mkTelegramTool(t)
	ag := af.addAgent("alpha")
	ch := cf.addTelegramChannel("telegram/alpha", ag.ID, "tok")

	r := tool.Execute(context.Background(), map[string]any{
		"action": "unlink", "channel_name": "telegram/alpha",
	})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	if _, err := cf.Get(context.Background(), ch.ID); err == nil {
		t.Errorf("channel should be deleted")
	}
}

func TestAgentTelegram_Unlink_ByID(t *testing.T) {
	tool, af, cf, _ := mkTelegramTool(t)
	ag := af.addAgent("alpha")
	ch := cf.addTelegramChannel("telegram/alpha", ag.ID, "tok")

	r := tool.Execute(context.Background(), map[string]any{
		"action": "unlink", "channel_id": ch.ID.String(),
	})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
}

func TestAgentTelegram_Unlink_NotFound(t *testing.T) {
	tool, _, _, _ := mkTelegramTool(t)
	r := tool.Execute(context.Background(), map[string]any{
		"action": "unlink", "channel_name": "telegram/ghost",
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "not found") {
		t.Fatalf("expected not-found, got: %+v", r)
	}
}

func TestAgentTelegram_Unlink_MissingArgs(t *testing.T) {
	tool, _, _, _ := mkTelegramTool(t)
	r := tool.Execute(context.Background(), map[string]any{"action": "unlink"})
	if !r.IsError || !strings.Contains(r.ForLLM, "channel_id") {
		t.Fatalf("expected required-arg error, got: %+v", r)
	}
}

// --- parent resolution ---

func TestAgentTelegram_ResolveParent_FromCallerAgent(t *testing.T) {
	tool, af, cf, _ := mkTelegramTool(t)
	forge := af.addAgent("forge")
	target := af.addAgent("ath-copywriter")
	cf.addTelegramChannel("telegram/forge", forge.ID, "parent_tok")

	// Put caller agent ID in context; resolver should find the channel automatically.
	ctx := store.WithAgentID(context.Background(), forge.ID)

	r := tool.Execute(ctx, map[string]any{
		"action":             "poll_managed_bot_token",
		"suggested_username": "ath_copywriter_ath_bot",
		"agent_id":           "ath-copywriter",
	})
	// We didn't set tokenForPoll, so fixture will succeed with empty token —
	// which the tool rejects with "empty token" error. That's fine — what
	// we're testing is that parent resolution worked (we didn't get
	// parent-not-found).
	if r.IsError && strings.Contains(r.ForLLM, "parent channel") {
		t.Errorf("parent resolution from context failed: %s", r.ForLLM)
	}
	_ = target
}

func TestAgentTelegram_ResolveParent_NoneFound(t *testing.T) {
	tool, _, _, _ := mkTelegramTool(t)
	r := tool.Execute(context.Background(), map[string]any{
		"action":             "request_managed_bot",
		"suggested_username": "test_ath_bot",
		"operator_chat_id":   "12345",
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "parent") {
		t.Fatalf("expected parent error, got: %+v", r)
	}
}

// --- HTTP envelope handling ---

func TestAgentTelegram_HTTPError_BadEnvelope(t *testing.T) {
	tool, af, cf, fx := mkTelegramTool(t)
	ag := af.addAgent("forge")
	cf.addTelegramChannel("telegram/forge", ag.ID, "tok")

	// Override fixture handler to return non-JSON garbage on getMe.
	fx.server.Close()
	fx.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "not json at all")
	}))
	tool.SetAPIBase(fx.server.URL)
	t.Cleanup(func() { fx.server.Close() })

	r := tool.Execute(context.Background(), map[string]any{
		"action":              "request_managed_bot",
		"suggested_username":  "test_ath_bot",
		"operator_chat_id":    "12345",
		"parent_channel_name": "telegram/forge",
	})
	if !r.IsError {
		t.Fatalf("expected error, got: %+v", r)
	}
}
