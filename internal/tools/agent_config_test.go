package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// configFake is an in-memory AgentStore for agent_config tests. Embeds
// stubAgentStore from context_file_interceptor_test.go for no-op coverage,
// overrides the few methods we exercise with real behavior.
type configFake struct {
	*stubAgentStore
	mu          sync.Mutex
	byKey       map[string]*store.AgentData
	byID        map[uuid.UUID]*store.AgentData
	files       map[uuid.UUID]map[string]string
	updateErr   error
	rereadErr   error
	updateCalls int
}

func newConfigFake() *configFake {
	return &configFake{
		stubAgentStore: &stubAgentStore{},
		byKey:          make(map[string]*store.AgentData),
		byID:           make(map[uuid.UUID]*store.AgentData),
		files:          make(map[uuid.UUID]map[string]string),
	}
}

func (f *configFake) addAgent(key string) *store.AgentData {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := uuid.New()
	ag := &store.AgentData{
		AgentKey:           key,
		DisplayName:        key,
		Provider:           "claude-cli",
		Model:              "opus",
		AgentType:          store.AgentTypePredefined,
		Status:             store.AgentStatusActive,
		ContextWindow:      200000,
		MaxToolIterations:  25,
		ThinkingLevel:      "low",
		SelfEvolve:         false,
		SkillEvolve:        false,
		MemoryConfig:       json.RawMessage(`{"enabled":true}`),
		ToolsConfig:        json.RawMessage(`{"allow":["read_file","write_file"]}`),
	}
	ag.ID = id
	f.byKey[key] = ag
	f.byID[id] = ag
	f.files[id] = make(map[string]string)
	return ag
}

func (f *configFake) GetByKey(_ context.Context, key string) (*store.AgentData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ag, ok := f.byKey[key]; ok {
		cp := *ag
		return &cp, nil
	}
	return nil, errors.New("agent not found: " + key)
}

func (f *configFake) GetByID(_ context.Context, id uuid.UUID) (*store.AgentData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ag, ok := f.byID[id]; ok {
		if f.rereadErr != nil && f.updateCalls > 0 {
			err := f.rereadErr
			f.rereadErr = nil
			return nil, err
		}
		cp := *ag
		return &cp, nil
	}
	return nil, errors.New("agent not found: " + id.String())
}

func (f *configFake) Update(_ context.Context, id uuid.UUID, updates map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateCalls++
	if f.updateErr != nil {
		err := f.updateErr
		f.updateErr = nil
		return err
	}
	ag, ok := f.byID[id]
	if !ok {
		return errors.New("agent not found")
	}
	// Apply fields we care about for tests.
	for k, v := range updates {
		switch k {
		case "display_name":
			ag.DisplayName, _ = v.(string)
		case "provider":
			ag.Provider, _ = v.(string)
		case "model":
			ag.Model, _ = v.(string)
		case "thinking_level":
			ag.ThinkingLevel, _ = v.(string)
		case "status":
			ag.Status, _ = v.(string)
		case "context_window":
			if n, ok := asInt(v); ok {
				ag.ContextWindow = n
			}
		case "max_tool_iterations":
			if n, ok := asInt(v); ok {
				ag.MaxToolIterations = n
			}
		case "self_evolve":
			ag.SelfEvolve, _ = v.(bool)
		case "skill_evolve":
			ag.SkillEvolve, _ = v.(bool)
		case "is_default":
			ag.IsDefault, _ = v.(bool)
		case "tools_config":
			if m, ok := v.(map[string]any); ok {
				b, _ := json.Marshal(m)
				ag.ToolsConfig = b
			}
		case "reasoning_config":
			if m, ok := v.(map[string]any); ok {
				b, _ := json.Marshal(m)
				ag.ReasoningConfig = b
			}
		case "memory_config":
			if m, ok := v.(map[string]any); ok {
				b, _ := json.Marshal(m)
				ag.MemoryConfig = b
			}
		case "other_config":
			if m, ok := v.(map[string]any); ok {
				b, _ := json.Marshal(m)
				ag.OtherConfig = b
			}
		case "agent_key":
			old := ag.AgentKey
			ag.AgentKey, _ = v.(string)
			delete(f.byKey, old)
			f.byKey[ag.AgentKey] = ag
		case "agent_description":
			ag.AgentDescription, _ = v.(string)
		case "frontmatter":
			ag.Frontmatter, _ = v.(string)
		case "emoji":
			ag.Emoji, _ = v.(string)
		}
	}
	return nil
}

func (f *configFake) GetAgentContextFiles(_ context.Context, agentID uuid.UUID) ([]store.AgentContextFileData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []store.AgentContextFileData{}
	for name, content := range f.files[agentID] {
		out = append(out, store.AgentContextFileData{AgentID: agentID, FileName: name, Content: content})
	}
	return out, nil
}

func (f *configFake) SetAgentContextFile(_ context.Context, agentID uuid.UUID, fileName, content string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.files[agentID]; !ok {
		f.files[agentID] = make(map[string]string)
	}
	f.files[agentID][fileName] = content
	return nil
}

func mkConfigTool(t *testing.T) (*AgentConfigTool, *configFake) {
	t.Helper()
	fake := newConfigFake()
	tool := NewAgentConfigTool()
	tool.SetAgentStore(fake)
	// msgBus intentionally left nil — tool degrades gracefully and tests
	// don't need bus event observation for behavior verification.
	return tool, fake
}

// --- dispatch ---

func TestAgentConfig_NoStore(t *testing.T) {
	tool := NewAgentConfigTool()
	r := tool.Execute(context.Background(), map[string]any{"action": "read", "agent_id": "x"})
	if !r.IsError || !strings.Contains(r.ForLLM, "agent store not wired") {
		t.Fatalf("expected store-not-wired error, got: %+v", r)
	}
}

func TestAgentConfig_MissingAction(t *testing.T) {
	tool, _ := mkConfigTool(t)
	r := tool.Execute(context.Background(), map[string]any{})
	if !r.IsError || !strings.Contains(r.ForLLM, "action") {
		t.Fatalf("expected action error, got: %+v", r)
	}
}

func TestAgentConfig_UnknownAction(t *testing.T) {
	tool, _ := mkConfigTool(t)
	r := tool.Execute(context.Background(), map[string]any{"action": "blah"})
	if !r.IsError || !strings.Contains(r.ForLLM, "unknown action") {
		t.Fatalf("expected unknown-action, got: %+v", r)
	}
}

// --- read ---

func TestAgentConfig_Read_AgentNotFound(t *testing.T) {
	tool, _ := mkConfigTool(t)
	r := tool.Execute(context.Background(), map[string]any{"action": "read", "agent_id": "ghost"})
	if !r.IsError || !strings.Contains(r.ForLLM, "agent not found") {
		t.Fatalf("expected agent-not-found, got: %+v", r)
	}
}

func TestAgentConfig_Read_OK(t *testing.T) {
	tool, fake := mkConfigTool(t)
	fake.addAgent("alpha")
	r := tool.Execute(context.Background(), map[string]any{"action": "read", "agent_id": "alpha"})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	// Should be valid JSON containing the agent's fields
	var parsed map[string]any
	if err := json.Unmarshal([]byte(r.ForLLM), &parsed); err != nil {
		t.Fatalf("response not valid JSON: %v\n%s", err, r.ForLLM)
	}
	if parsed["agent_id"] != "alpha" {
		t.Errorf("agent_id mismatch: %v", parsed["agent_id"])
	}
	if parsed["provider"] != "claude-cli" {
		t.Errorf("provider mismatch: %v", parsed["provider"])
	}
	// JSONB field should be parsed object, not string
	tc, ok := parsed["tools_config"].(map[string]any)
	if !ok {
		t.Fatalf("tools_config should be object, got %T: %v", parsed["tools_config"], parsed["tools_config"])
	}
	allow, _ := tc["allow"].([]any)
	if len(allow) != 2 {
		t.Errorf("expected 2 allow entries, got: %v", allow)
	}
}

func TestAgentConfig_Read_ByUUID(t *testing.T) {
	tool, fake := mkConfigTool(t)
	ag := fake.addAgent("alpha")
	r := tool.Execute(context.Background(), map[string]any{"action": "read", "agent_id": ag.ID.String()})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
}

// --- update: dispatch + filtering ---

func TestAgentConfig_Update_AgentNotFound(t *testing.T) {
	tool, _ := mkConfigTool(t)
	r := tool.Execute(context.Background(), map[string]any{"action": "update", "agent_id": "ghost", "display_name": "Ghost"})
	if !r.IsError || !strings.Contains(r.ForLLM, "agent not found") {
		t.Fatalf("expected agent-not-found, got: %+v", r)
	}
}

func TestAgentConfig_Update_NoFields(t *testing.T) {
	tool, fake := mkConfigTool(t)
	fake.addAgent("alpha")
	r := tool.Execute(context.Background(), map[string]any{"action": "update", "agent_id": "alpha"})
	if !r.IsError || !strings.Contains(r.ForLLM, "at least one") {
		t.Fatalf("expected no-fields error, got: %+v", r)
	}
}

func TestAgentConfig_Update_OnlyNonMutableFields(t *testing.T) {
	// Submitting only non-mutable fields (agent_type, tenant_id, etc.) should
	// behave like an empty update — they get silently filtered.
	tool, fake := mkConfigTool(t)
	fake.addAgent("alpha")
	r := tool.Execute(context.Background(), map[string]any{
		"action":     "update",
		"agent_id":   "alpha",
		"agent_type": "open",
		"tenant_id":  "ignored",
		"id":         "ignored",
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "at least one") {
		t.Fatalf("expected no-fields error after filtering, got: %+v", r)
	}
}

func TestAgentConfig_Update_MutableFieldChange(t *testing.T) {
	tool, fake := mkConfigTool(t)
	fake.addAgent("alpha")
	r := tool.Execute(context.Background(), map[string]any{
		"action": "update", "agent_id": "alpha",
		"thinking_level": "medium",
	})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(r.ForLLM), &resp); err != nil {
		t.Fatalf("invalid JSON response: %v\n%s", err, r.ForLLM)
	}
	changed, _ := resp["fields_changed"].([]any)
	if len(changed) != 1 || changed[0] != "thinking_level" {
		t.Errorf("expected fields_changed=[thinking_level], got: %v", changed)
	}
	before, _ := resp["before"].(map[string]any)
	after, _ := resp["after"].(map[string]any)
	if before["thinking_level"] != "low" {
		t.Errorf("expected before.thinking_level=low, got: %v", before["thinking_level"])
	}
	if after["thinking_level"] != "medium" {
		t.Errorf("expected after.thinking_level=medium, got: %v", after["thinking_level"])
	}
}

func TestAgentConfig_Update_IgnoredFieldsReported(t *testing.T) {
	tool, fake := mkConfigTool(t)
	fake.addAgent("alpha")
	r := tool.Execute(context.Background(), map[string]any{
		"action": "update", "agent_id": "alpha",
		"thinking_level": "medium",
		"agent_type":     "open", // not mutable, should be reported in ignored
	})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	var resp map[string]any
	_ = json.Unmarshal([]byte(r.ForLLM), &resp)
	ignored, _ := resp["ignored_fields"].([]any)
	if len(ignored) != 1 || ignored[0] != "agent_type" {
		t.Errorf("expected ignored_fields=[agent_type], got: %v", ignored)
	}
}

// --- update: validation ---

func TestAgentConfig_Update_InvalidAgentKeySlug(t *testing.T) {
	tool, fake := mkConfigTool(t)
	fake.addAgent("alpha")
	r := tool.Execute(context.Background(), map[string]any{
		"action": "update", "agent_id": "alpha",
		"agent_key": "Invalid:Key",
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "slug format") {
		t.Fatalf("expected slug error, got: %+v", r)
	}
}

func TestAgentConfig_Update_InvalidThinkingLevel(t *testing.T) {
	tool, fake := mkConfigTool(t)
	fake.addAgent("alpha")
	r := tool.Execute(context.Background(), map[string]any{
		"action": "update", "agent_id": "alpha",
		"thinking_level": "extreme",
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "thinking_level") {
		t.Fatalf("expected enum error, got: %+v", r)
	}
}

func TestAgentConfig_Update_InvalidStatus(t *testing.T) {
	tool, fake := mkConfigTool(t)
	fake.addAgent("alpha")
	r := tool.Execute(context.Background(), map[string]any{
		"action": "update", "agent_id": "alpha",
		"status": "summoning",
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "reserved") {
		t.Fatalf("expected status-reserved error, got: %+v", r)
	}
}

func TestAgentConfig_Update_StatusAllowed(t *testing.T) {
	tool, fake := mkConfigTool(t)
	fake.addAgent("alpha")
	r := tool.Execute(context.Background(), map[string]any{
		"action": "update", "agent_id": "alpha",
		"status": "summon_failed",
	})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
}

func TestAgentConfig_Update_InvalidReasoningEffort(t *testing.T) {
	tool, fake := mkConfigTool(t)
	fake.addAgent("alpha")
	r := tool.Execute(context.Background(), map[string]any{
		"action": "update", "agent_id": "alpha",
		"reasoning_config": map[string]any{"effort": "ultra"},
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "reasoning_config.effort") {
		t.Fatalf("expected effort error, got: %+v", r)
	}
}

func TestAgentConfig_Update_NonPositiveInt(t *testing.T) {
	tool, fake := mkConfigTool(t)
	fake.addAgent("alpha")
	r := tool.Execute(context.Background(), map[string]any{
		"action": "update", "agent_id": "alpha",
		"context_window": 0,
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "> 0") {
		t.Fatalf("expected positive-int error, got: %+v", r)
	}
}

func TestAgentConfig_Update_FloatToIntCoercion(t *testing.T) {
	// JSON unmarshal produces float64 — integer-valued floats should be
	// accepted, non-integer floats rejected.
	tool, fake := mkConfigTool(t)
	fake.addAgent("alpha")
	r := tool.Execute(context.Background(), map[string]any{
		"action": "update", "agent_id": "alpha",
		"context_window": 200000.0,
	})
	if r.IsError {
		t.Fatalf("integer-valued float should be accepted: %s", r.ForLLM)
	}

	r = tool.Execute(context.Background(), map[string]any{
		"action": "update", "agent_id": "alpha",
		"context_window": 1.5,
	})
	if !r.IsError {
		t.Fatalf("non-integer float should be rejected")
	}
}

func TestAgentConfig_Update_InvalidJSONBField(t *testing.T) {
	tool, fake := mkConfigTool(t)
	fake.addAgent("alpha")
	r := tool.Execute(context.Background(), map[string]any{
		"action": "update", "agent_id": "alpha",
		"tools_config": 42, // not an object/array/JSON string
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "tools_config") {
		t.Fatalf("expected JSONB shape error, got: %+v", r)
	}
}

func TestAgentConfig_Update_JSONBAsString(t *testing.T) {
	// JSON-encoded string form should be accepted.
	tool, fake := mkConfigTool(t)
	fake.addAgent("alpha")
	r := tool.Execute(context.Background(), map[string]any{
		"action": "update", "agent_id": "alpha",
		"tools_config": `{"allow":["bash"]}`,
	})
	if r.IsError {
		t.Fatalf("JSON-encoded string should be accepted: %s", r.ForLLM)
	}
}

// --- update: side-effects ---

func TestAgentConfig_Update_DisplayNameSyncsIdentity(t *testing.T) {
	tool, fake := mkConfigTool(t)
	ag := fake.addAgent("alpha")
	fake.files[ag.ID]["IDENTITY.md"] = "# Identity\nName: Old Name\n"

	r := tool.Execute(context.Background(), map[string]any{
		"action": "update", "agent_id": "alpha",
		"display_name": "New Name",
	})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	got := fake.files[ag.ID]["IDENTITY.md"]
	if !strings.Contains(got, "New Name") {
		t.Errorf("expected IDENTITY.md to contain 'New Name', got: %q", got)
	}
	if strings.Contains(got, "Old Name") {
		t.Errorf("expected IDENTITY.md to NOT contain 'Old Name', got: %q", got)
	}
}

func TestAgentConfig_Update_AgentKeyRename(t *testing.T) {
	tool, fake := mkConfigTool(t)
	ag := fake.addAgent("alpha")
	r := tool.Execute(context.Background(), map[string]any{
		"action": "update", "agent_id": "alpha",
		"agent_key": "beta",
	})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	// After rename, GetByKey for old name fails, new name succeeds.
	if _, err := fake.GetByKey(context.Background(), "alpha"); err == nil {
		t.Error("expected alpha to no longer exist")
	}
	got, err := fake.GetByKey(context.Background(), "beta")
	if err != nil {
		t.Fatalf("expected beta to exist: %v", err)
	}
	if got.ID != ag.ID {
		t.Errorf("expected same UUID after rename, got different")
	}
}

func TestAgentConfig_Update_StoreError(t *testing.T) {
	tool, fake := mkConfigTool(t)
	fake.addAgent("alpha")
	fake.updateErr = errors.New("simulated db failure")
	r := tool.Execute(context.Background(), map[string]any{
		"action": "update", "agent_id": "alpha",
		"thinking_level": "high",
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "update failed") {
		t.Fatalf("expected update failed error, got: %+v", r)
	}
}

func TestAgentConfig_Update_RereadError(t *testing.T) {
	tool, fake := mkConfigTool(t)
	fake.addAgent("alpha")
	fake.rereadErr = errors.New("simulated re-read failure")
	r := tool.Execute(context.Background(), map[string]any{
		"action": "update", "agent_id": "alpha",
		"thinking_level": "high",
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "re-read failed") {
		t.Fatalf("expected re-read failed error, got: %+v", r)
	}
}

func TestAgentConfig_Update_BoolField(t *testing.T) {
	tool, fake := mkConfigTool(t)
	fake.addAgent("alpha")
	r := tool.Execute(context.Background(), map[string]any{
		"action": "update", "agent_id": "alpha",
		"self_evolve": true,
	})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	var resp map[string]any
	_ = json.Unmarshal([]byte(r.ForLLM), &resp)
	after, _ := resp["after"].(map[string]any)
	if after["self_evolve"] != true {
		t.Errorf("expected self_evolve=true after update, got: %v", after["self_evolve"])
	}
}

func TestAgentConfig_Update_DeepConfigReplace(t *testing.T) {
	tool, fake := mkConfigTool(t)
	fake.addAgent("alpha")
	r := tool.Execute(context.Background(), map[string]any{
		"action": "update", "agent_id": "alpha",
		"tools_config": map[string]any{"allow": []any{"bash", "edit"}},
	})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	// Verify post-state via read
	rr := tool.Execute(context.Background(), map[string]any{"action": "read", "agent_id": "alpha"})
	var parsed map[string]any
	_ = json.Unmarshal([]byte(rr.ForLLM), &parsed)
	tc, _ := parsed["tools_config"].(map[string]any)
	allow, _ := tc["allow"].([]any)
	if len(allow) != 2 || allow[0] != "bash" {
		t.Errorf("expected tools_config.allow=[bash, edit], got: %v", allow)
	}
}
