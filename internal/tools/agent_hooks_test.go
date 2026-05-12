package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	hookspkg "github.com/nextlevelbuilder/goclaw/internal/hooks"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// hookAgentFake — minimal AgentStore for hook tests.
type hookAgentFake struct {
	*stubAgentStore
	mu    sync.Mutex
	byKey map[string]*store.AgentData
	byID  map[uuid.UUID]*store.AgentData
}

func newHookAgentFake() *hookAgentFake {
	return &hookAgentFake{
		stubAgentStore: &stubAgentStore{},
		byKey:          make(map[string]*store.AgentData),
		byID:           make(map[uuid.UUID]*store.AgentData),
	}
}

func (f *hookAgentFake) addAgent(key string) *store.AgentData {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := uuid.New()
	ag := &store.AgentData{AgentKey: key}
	ag.ID = id
	f.byKey[key] = ag
	f.byID[id] = ag
	return ag
}

func (f *hookAgentFake) GetByKey(_ context.Context, key string) (*store.AgentData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ag, ok := f.byKey[key]; ok {
		return ag, nil
	}
	return nil, errors.New("agent not found: " + key)
}

func (f *hookAgentFake) GetByID(_ context.Context, id uuid.UUID) (*store.AgentData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ag, ok := f.byID[id]; ok {
		return ag, nil
	}
	return nil, errors.New("agent not found")
}

// fakeHookStore — minimal HookStore impl.
type fakeHookStore struct {
	mu         sync.Mutex
	hooks      map[uuid.UUID]*hookspkg.HookConfig
	junction   map[uuid.UUID][]uuid.UUID
	createErr  error
	updateErr  error
	deleteErr  error
}

func newFakeHookStore() *fakeHookStore {
	return &fakeHookStore{
		hooks:    make(map[uuid.UUID]*hookspkg.HookConfig),
		junction: make(map[uuid.UUID][]uuid.UUID),
	}
}

func (f *fakeHookStore) Create(_ context.Context, cfg hookspkg.HookConfig) (uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		err := f.createErr
		f.createErr = nil
		return uuid.Nil, err
	}
	id := uuid.New()
	cfg.ID = id
	cfg.Version = 1
	f.hooks[id] = &cfg
	if len(cfg.AgentIDs) > 0 {
		f.junction[id] = append([]uuid.UUID{}, cfg.AgentIDs...)
	}
	return id, nil
}

func (f *fakeHookStore) GetByID(_ context.Context, id uuid.UUID) (*hookspkg.HookConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if h, ok := f.hooks[id]; ok {
		cp := *h
		return &cp, nil
	}
	return nil, nil
}

func (f *fakeHookStore) List(_ context.Context, filter hookspkg.ListFilter) ([]hookspkg.HookConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []hookspkg.HookConfig{}
	for _, h := range f.hooks {
		if filter.Enabled != nil && h.Enabled != *filter.Enabled {
			continue
		}
		if filter.Event != nil && h.Event != *filter.Event {
			continue
		}
		if filter.Scope != nil && h.Scope != *filter.Scope {
			continue
		}
		if filter.AgentID != nil {
			matched := false
			for _, aid := range f.junction[h.ID] {
				if aid == *filter.AgentID {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		out = append(out, *h)
	}
	return out, nil
}

func (f *fakeHookStore) Update(_ context.Context, id uuid.UUID, updates map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updateErr != nil {
		err := f.updateErr
		f.updateErr = nil
		return err
	}
	h, ok := f.hooks[id]
	if !ok {
		return errors.New("not found")
	}
	for k, v := range updates {
		switch k {
		case "enabled":
			h.Enabled, _ = v.(bool)
		case "name":
			h.Name, _ = v.(string)
		case "priority":
			if n, ok := asInt(v); ok {
				h.Priority = n
			}
		case "matcher":
			h.Matcher, _ = v.(string)
		case "timeout_ms":
			if n, ok := asInt(v); ok {
				h.TimeoutMS = n
			}
		case "config":
			if m, ok := v.(map[string]any); ok {
				h.Config = m
			}
		}
	}
	h.Version++
	return nil
}

func (f *fakeHookStore) Delete(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		err := f.deleteErr
		f.deleteErr = nil
		return err
	}
	delete(f.hooks, id)
	delete(f.junction, id)
	return nil
}

func (f *fakeHookStore) ResolveForEvent(_ context.Context, _ hookspkg.Event) ([]hookspkg.HookConfig, error) {
	return nil, nil
}

func (f *fakeHookStore) WriteExecution(_ context.Context, _ hookspkg.HookExecution) error {
	return nil
}

func (f *fakeHookStore) SetHookAgents(_ context.Context, hookID uuid.UUID, agentIDs []uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.junction[hookID] = append([]uuid.UUID{}, agentIDs...)
	return nil
}

func (f *fakeHookStore) GetHookAgents(_ context.Context, hookID uuid.UUID) ([]uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]uuid.UUID, len(f.junction[hookID]))
	copy(out, f.junction[hookID])
	return out, nil
}

// --- harness ---

func mkHooksTool(t *testing.T) (*AgentHooksTool, *hookAgentFake, *fakeHookStore) {
	t.Helper()
	af := newHookAgentFake()
	hf := newFakeHookStore()
	tool := NewAgentHooksTool()
	tool.SetAgentStore(af)
	tool.SetHookStore(hf)
	return tool, af, hf
}

// --- dispatch ---

func TestAgentHooks_NoStore(t *testing.T) {
	tool := NewAgentHooksTool()
	r := tool.Execute(context.Background(), map[string]any{"action": "list"})
	if !r.IsError {
		t.Fatalf("expected error, got: %+v", r)
	}
}

func TestAgentHooks_UnknownAction(t *testing.T) {
	tool, _, _ := mkHooksTool(t)
	r := tool.Execute(context.Background(), map[string]any{"action": "blah"})
	if !r.IsError || !strings.Contains(r.ForLLM, "unknown action") {
		t.Fatalf("expected unknown-action, got: %+v", r)
	}
}

// --- create ---

func TestAgentHooks_Create_MissingEvent(t *testing.T) {
	tool, _, _ := mkHooksTool(t)
	r := tool.Execute(context.Background(), map[string]any{
		"action":       "create",
		"handler_type": "command",
		"scope":        "tenant",
		"config":       map[string]any{"command": "/bin/true"},
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "event is required") {
		t.Fatalf("expected event-required, got: %+v", r)
	}
}

func TestAgentHooks_Create_InvalidEvent(t *testing.T) {
	tool, _, _ := mkHooksTool(t)
	r := tool.Execute(context.Background(), map[string]any{
		"action": "create", "event": "frobnicate",
		"handler_type": "command", "scope": "tenant",
		"config": map[string]any{"command": "/bin/true"},
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "event") {
		t.Fatalf("expected event-invalid, got: %+v", r)
	}
}

func TestAgentHooks_Create_GlobalScopeBlocked(t *testing.T) {
	tool, _, _ := mkHooksTool(t)
	r := tool.Execute(context.Background(), map[string]any{
		"action": "create", "event": "stop",
		"handler_type": "command", "scope": "global",
		"config": map[string]any{"command": "/bin/true"},
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "global") {
		t.Fatalf("expected global-blocked, got: %+v", r)
	}
}

func TestAgentHooks_Create_AgentScopeRequiresAgentIDs(t *testing.T) {
	tool, _, _ := mkHooksTool(t)
	r := tool.Execute(context.Background(), map[string]any{
		"action": "create", "event": "pre_tool_use",
		"handler_type": "command", "scope": "agent",
		"config": map[string]any{"command": "/bin/true"},
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "agent_ids") {
		t.Fatalf("expected agent_ids-required, got: %+v", r)
	}
}

func TestAgentHooks_Create_TenantScopeOK(t *testing.T) {
	tool, _, hf := mkHooksTool(t)
	r := tool.Execute(context.Background(), map[string]any{
		"action":       "create",
		"event":        "stop",
		"handler_type": "command",
		"scope":        "tenant",
		"name":         "test-stop",
		"config":       map[string]any{"command": "/bin/true"},
	})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	var resp map[string]any
	_ = json.Unmarshal([]byte(r.ForLLM), &resp)
	if resp["created"] != true {
		t.Errorf("expected created=true, got: %v", resp["created"])
	}
	if len(hf.hooks) != 1 {
		t.Errorf("expected 1 hook stored, got: %d", len(hf.hooks))
	}
}

func TestAgentHooks_Create_AgentScopeWithSlug(t *testing.T) {
	tool, af, hf := mkHooksTool(t)
	ag := af.addAgent("alpha")
	r := tool.Execute(context.Background(), map[string]any{
		"action":       "create",
		"event":        "pre_tool_use",
		"handler_type": "command",
		"scope":        "agent",
		"agent_ids":    []any{"alpha"},
		"matcher":      "^bash$",
		"config":       map[string]any{"command": "/usr/bin/forge-validate"},
	})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	if len(hf.hooks) != 1 {
		t.Fatalf("expected 1 hook, got: %d", len(hf.hooks))
	}
	for _, h := range hf.hooks {
		if len(hf.junction[h.ID]) != 1 || hf.junction[h.ID][0] != ag.ID {
			t.Errorf("junction not populated correctly: %v", hf.junction[h.ID])
		}
	}
}

func TestAgentHooks_Create_BadTimeout(t *testing.T) {
	tool, _, _ := mkHooksTool(t)
	r := tool.Execute(context.Background(), map[string]any{
		"action": "create", "event": "stop",
		"handler_type": "command", "scope": "tenant",
		"config":     map[string]any{"command": "/bin/true"},
		"timeout_ms": 0,
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "timeout_ms") {
		t.Fatalf("expected timeout error, got: %+v", r)
	}
}

func TestAgentHooks_Create_StoreError(t *testing.T) {
	tool, _, hf := mkHooksTool(t)
	hf.createErr = errors.New("simulated DB failure")
	r := tool.Execute(context.Background(), map[string]any{
		"action": "create", "event": "stop",
		"handler_type": "command", "scope": "tenant",
		"config": map[string]any{"command": "/bin/true"},
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "create failed") {
		t.Fatalf("expected store error, got: %+v", r)
	}
}

// --- list / get ---

func TestAgentHooks_List_Empty(t *testing.T) {
	tool, _, _ := mkHooksTool(t)
	r := tool.Execute(context.Background(), map[string]any{"action": "list"})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	var resp map[string]any
	_ = json.Unmarshal([]byte(r.ForLLM), &resp)
	if resp["count"].(float64) != 0 {
		t.Errorf("expected count=0, got: %v", resp["count"])
	}
}

func TestAgentHooks_List_FilterByAgent(t *testing.T) {
	tool, af, hf := mkHooksTool(t)
	ag := af.addAgent("alpha")
	other := af.addAgent("beta")
	id1, _ := hf.Create(context.Background(), hookspkg.HookConfig{
		Event: "stop", Scope: "agent", HandlerType: "command", Enabled: true,
		AgentIDs: []uuid.UUID{ag.ID},
	})
	_, _ = hf.Create(context.Background(), hookspkg.HookConfig{
		Event: "stop", Scope: "agent", HandlerType: "command", Enabled: true,
		AgentIDs: []uuid.UUID{other.ID},
	})

	r := tool.Execute(context.Background(), map[string]any{"action": "list", "agent_id": "alpha"})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	var resp map[string]any
	_ = json.Unmarshal([]byte(r.ForLLM), &resp)
	if resp["count"].(float64) != 1 {
		t.Errorf("expected count=1, got: %v", resp["count"])
	}
	hooksList, _ := resp["hooks"].([]any)
	hook0, _ := hooksList[0].(map[string]any)
	if hook0["hook_id"] != id1.String() {
		t.Errorf("expected hook_id %s, got: %v", id1.String(), hook0["hook_id"])
	}
}

func TestAgentHooks_Get_NotFound(t *testing.T) {
	tool, _, _ := mkHooksTool(t)
	r := tool.Execute(context.Background(), map[string]any{
		"action": "get", "hook_id": uuid.New().String(),
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "hook not found") {
		t.Fatalf("expected not-found, got: %+v", r)
	}
}

func TestAgentHooks_Get_OK(t *testing.T) {
	tool, af, hf := mkHooksTool(t)
	ag := af.addAgent("alpha")
	id, _ := hf.Create(context.Background(), hookspkg.HookConfig{
		Event: "pre_tool_use", Scope: "agent", HandlerType: "prompt",
		Enabled: true, Matcher: "^bash$",
		Config:   map[string]any{"prompt": "validate this"},
		AgentIDs: []uuid.UUID{ag.ID},
	})
	r := tool.Execute(context.Background(), map[string]any{"action": "get", "hook_id": id.String()})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	var resp map[string]any
	_ = json.Unmarshal([]byte(r.ForLLM), &resp)
	if resp["matcher"] != "^bash$" {
		t.Errorf("matcher mismatch: %v", resp["matcher"])
	}
	agentIDs, _ := resp["agent_ids"].([]any)
	if len(agentIDs) != 1 {
		t.Errorf("expected 1 agent_id in response, got: %v", agentIDs)
	}
}

// --- update / toggle ---

func TestAgentHooks_Update_NonMutableField(t *testing.T) {
	tool, _, hf := mkHooksTool(t)
	id, _ := hf.Create(context.Background(), hookspkg.HookConfig{Event: "stop", Scope: "tenant", HandlerType: "command", Enabled: true})

	r := tool.Execute(context.Background(), map[string]any{
		"action":  "update",
		"hook_id": id.String(),
		"updates": map[string]any{"source": "builtin", "version": 99},
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "no mutable fields") {
		t.Fatalf("expected no-mutable error, got: %+v", r)
	}
}

func TestAgentHooks_Update_OK(t *testing.T) {
	tool, _, hf := mkHooksTool(t)
	id, _ := hf.Create(context.Background(), hookspkg.HookConfig{Event: "stop", Scope: "tenant", HandlerType: "command", Enabled: true, Priority: 0})

	r := tool.Execute(context.Background(), map[string]any{
		"action":  "update",
		"hook_id": id.String(),
		"updates": map[string]any{"priority": 10, "name": "renamed"},
	})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	if hf.hooks[id].Priority != 10 || hf.hooks[id].Name != "renamed" {
		t.Errorf("update not applied: %+v", hf.hooks[id])
	}
}

func TestAgentHooks_Toggle_OK(t *testing.T) {
	tool, _, hf := mkHooksTool(t)
	id, _ := hf.Create(context.Background(), hookspkg.HookConfig{Event: "stop", Scope: "tenant", HandlerType: "command", Enabled: true})

	r := tool.Execute(context.Background(), map[string]any{
		"action": "toggle", "hook_id": id.String(), "enabled": false,
	})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	if hf.hooks[id].Enabled {
		t.Errorf("expected enabled=false after toggle")
	}
}

func TestAgentHooks_Toggle_MissingFlag(t *testing.T) {
	tool, _, hf := mkHooksTool(t)
	id, _ := hf.Create(context.Background(), hookspkg.HookConfig{Event: "stop", Scope: "tenant", HandlerType: "command", Enabled: true})
	r := tool.Execute(context.Background(), map[string]any{
		"action": "toggle", "hook_id": id.String(),
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "enabled") {
		t.Fatalf("expected enabled-required error, got: %+v", r)
	}
}

// --- delete ---

func TestAgentHooks_Delete_OK(t *testing.T) {
	tool, _, hf := mkHooksTool(t)
	id, _ := hf.Create(context.Background(), hookspkg.HookConfig{Event: "stop", Scope: "tenant", HandlerType: "command"})
	r := tool.Execute(context.Background(), map[string]any{"action": "delete", "hook_id": id.String()})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	if _, ok := hf.hooks[id]; ok {
		t.Error("hook should be deleted")
	}
}

func TestAgentHooks_Delete_BadID(t *testing.T) {
	tool, _, _ := mkHooksTool(t)
	r := tool.Execute(context.Background(), map[string]any{"action": "delete", "hook_id": "not-a-uuid"})
	if !r.IsError || !strings.Contains(r.ForLLM, "UUID") {
		t.Fatalf("expected UUID error, got: %+v", r)
	}
}

// --- set_agents ---

func TestAgentHooks_SetAgents_OK(t *testing.T) {
	tool, af, hf := mkHooksTool(t)
	a1 := af.addAgent("alpha")
	a2 := af.addAgent("beta")
	id, _ := hf.Create(context.Background(), hookspkg.HookConfig{Event: "pre_tool_use", Scope: "agent", HandlerType: "command", AgentIDs: []uuid.UUID{a1.ID}})

	r := tool.Execute(context.Background(), map[string]any{
		"action": "set_agents", "hook_id": id.String(),
		"agent_ids": []any{"alpha", "beta"},
	})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	if len(hf.junction[id]) != 2 {
		t.Fatalf("expected 2 agents in junction, got: %v", hf.junction[id])
	}
	// Verify both agents are bound
	found1, found2 := false, false
	for _, aid := range hf.junction[id] {
		if aid == a1.ID {
			found1 = true
		}
		if aid == a2.ID {
			found2 = true
		}
	}
	if !found1 || !found2 {
		t.Errorf("junction missing expected agent IDs")
	}
}

func TestAgentHooks_SetAgents_UnknownAgent(t *testing.T) {
	tool, _, hf := mkHooksTool(t)
	id, _ := hf.Create(context.Background(), hookspkg.HookConfig{Event: "pre_tool_use", Scope: "agent", HandlerType: "command"})
	r := tool.Execute(context.Background(), map[string]any{
		"action": "set_agents", "hook_id": id.String(),
		"agent_ids": []any{"ghost"},
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "agent not found") {
		t.Fatalf("expected agent-not-found, got: %+v", r)
	}
}
