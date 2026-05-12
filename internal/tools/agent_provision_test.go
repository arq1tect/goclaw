package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/bootstrap"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// provisionFake is an in-memory AgentStore for agent_provision tests.
// Embeds stubAgentStore for no-op coverage of unused methods; overrides
// the methods agent_provision actually touches with real behavior.
type provisionFake struct {
	*stubAgentStore
	mu        sync.Mutex
	byKey     map[string]*store.AgentData
	byID      map[uuid.UUID]*store.AgentData
	files     map[uuid.UUID]map[string]string
	deleted   map[uuid.UUID]bool
	createErr error
	deleteErr error
}

func newProvisionFake() *provisionFake {
	return &provisionFake{
		stubAgentStore: &stubAgentStore{},
		byKey:          make(map[string]*store.AgentData),
		byID:           make(map[uuid.UUID]*store.AgentData),
		files:          make(map[uuid.UUID]map[string]string),
		deleted:        make(map[uuid.UUID]bool),
	}
}

func (f *provisionFake) Create(_ context.Context, agent *store.AgentData) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		err := f.createErr
		f.createErr = nil
		return err
	}
	if _, exists := f.byKey[agent.AgentKey]; exists {
		return errors.New("duplicate key: agent_key already exists")
	}
	if agent.ID == uuid.Nil {
		agent.ID = uuid.New()
	}
	// Store a copy so callers mutating their input don't affect stored state.
	cp := *agent
	f.byKey[agent.AgentKey] = &cp
	f.byID[agent.ID] = &cp
	f.files[agent.ID] = make(map[string]string)
	return nil
}

func (f *provisionFake) Delete(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		err := f.deleteErr
		f.deleteErr = nil
		return err
	}
	ag, ok := f.byID[id]
	if !ok {
		return errors.New("not found")
	}
	delete(f.byKey, ag.AgentKey)
	delete(f.byID, id)
	delete(f.files, id)
	f.deleted[id] = true
	return nil
}

func (f *provisionFake) GetByKey(_ context.Context, key string) (*store.AgentData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ag, ok := f.byKey[key]; ok {
		cp := *ag
		return &cp, nil
	}
	return nil, errors.New("agent not found: " + key)
}

func (f *provisionFake) GetByID(_ context.Context, id uuid.UUID) (*store.AgentData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ag, ok := f.byID[id]; ok {
		cp := *ag
		return &cp, nil
	}
	return nil, errors.New("agent not found")
}

func (f *provisionFake) GetAgentContextFiles(_ context.Context, agentID uuid.UUID) ([]store.AgentContextFileData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []store.AgentContextFileData{}
	for name, content := range f.files[agentID] {
		out = append(out, store.AgentContextFileData{AgentID: agentID, FileName: name, Content: content})
	}
	return out, nil
}

func (f *provisionFake) SetAgentContextFile(_ context.Context, agentID uuid.UUID, fileName, content string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.files[agentID]; !ok {
		f.files[agentID] = make(map[string]string)
	}
	f.files[agentID][fileName] = content
	return nil
}

func (f *provisionFake) List(_ context.Context, _ string) ([]store.AgentData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.AgentData, 0, len(f.byID))
	for _, ag := range f.byID {
		out = append(out, *ag)
	}
	return out, nil
}

func mkProvisionTool(t *testing.T) (*AgentProvisionTool, *provisionFake) {
	t.Helper()
	fake := newProvisionFake()
	tool := NewAgentProvisionTool()
	tool.SetAgentStore(fake)
	tool.SetDefaultWorkspace("data/workspace")
	return tool, fake
}

// --- dispatch ---

func TestAgentProvision_NoStore(t *testing.T) {
	tool := NewAgentProvisionTool()
	r := tool.Execute(context.Background(), map[string]any{"action": "list"})
	if !r.IsError || !strings.Contains(r.ForLLM, "agent store not wired") {
		t.Fatalf("expected store error, got: %+v", r)
	}
}

func TestAgentProvision_MissingAction(t *testing.T) {
	tool, _ := mkProvisionTool(t)
	r := tool.Execute(context.Background(), map[string]any{})
	if !r.IsError || !strings.Contains(r.ForLLM, "action") {
		t.Fatalf("expected action error, got: %+v", r)
	}
}

func TestAgentProvision_UnknownAction(t *testing.T) {
	tool, _ := mkProvisionTool(t)
	r := tool.Execute(context.Background(), map[string]any{"action": "spawn"})
	if !r.IsError || !strings.Contains(r.ForLLM, "unknown action") {
		t.Fatalf("expected unknown-action, got: %+v", r)
	}
}

// --- create: required params ---

func TestAgentProvision_Create_MissingAgentKey(t *testing.T) {
	tool, _ := mkProvisionTool(t)
	r := tool.Execute(context.Background(), map[string]any{
		"action": "create", "provider": "claude-cli",
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "agent_key is required") {
		t.Fatalf("expected agent_key required, got: %+v", r)
	}
}

func TestAgentProvision_Create_InvalidSlug(t *testing.T) {
	tool, _ := mkProvisionTool(t)
	r := tool.Execute(context.Background(), map[string]any{
		"action": "create", "agent_key": "Invalid Key", "provider": "claude-cli",
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "slug format") {
		t.Fatalf("expected slug error, got: %+v", r)
	}
}

func TestAgentProvision_Create_MissingProvider(t *testing.T) {
	tool, _ := mkProvisionTool(t)
	r := tool.Execute(context.Background(), map[string]any{
		"action": "create", "agent_key": "alpha",
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "provider is required") {
		t.Fatalf("expected provider required, got: %+v", r)
	}
}

func TestAgentProvision_Create_Duplicate(t *testing.T) {
	tool, fake := mkProvisionTool(t)
	_ = fake.Create(context.Background(), &store.AgentData{AgentKey: "alpha"})
	r := tool.Execute(context.Background(), map[string]any{
		"action": "create", "agent_key": "alpha", "provider": "claude-cli",
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "already exists") {
		t.Fatalf("expected duplicate error, got: %+v", r)
	}
}

func TestAgentProvision_Create_OpenAgentTypeRejected(t *testing.T) {
	tool, _ := mkProvisionTool(t)
	r := tool.Execute(context.Background(), map[string]any{
		"action": "create", "agent_key": "alpha", "provider": "claude-cli",
		"agent_type": "open",
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "agent_type") {
		t.Fatalf("expected agent_type error, got: %+v", r)
	}
}

// --- create: success paths ---

func TestAgentProvision_Create_Minimal(t *testing.T) {
	tool, fake := mkProvisionTool(t)
	r := tool.Execute(context.Background(), map[string]any{
		"action": "create", "agent_key": "alpha", "provider": "claude-cli",
	})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	if _, ok := fake.byKey["alpha"]; !ok {
		t.Fatal("agent not stored")
	}
	ag := fake.byKey["alpha"]
	if ag.Provider != "claude-cli" {
		t.Errorf("provider mismatch: %s", ag.Provider)
	}
	if ag.AgentType != store.AgentTypePredefined {
		t.Errorf("expected predefined, got: %s", ag.AgentType)
	}
	if ag.Workspace != "data/workspace/alpha" {
		t.Errorf("workspace mismatch: %s", ag.Workspace)
	}
	if !ag.RestrictToWorkspace {
		t.Errorf("restrict_to_workspace should default to true")
	}
	// MemoryConfig should have been defaulted to enabled.
	var mc map[string]any
	_ = json.Unmarshal(ag.MemoryConfig, &mc)
	if mc["enabled"] != true {
		t.Errorf("memory_config should default to enabled=true, got: %v", mc)
	}
}

func TestAgentProvision_Create_WithFullConfig(t *testing.T) {
	tool, fake := mkProvisionTool(t)
	r := tool.Execute(context.Background(), map[string]any{
		"action":         "create",
		"agent_key":      "beta",
		"display_name":   "Beta Agent",
		"provider":       "claude-cli",
		"model":          "opus",
		"thinking_level": "medium",
		"context_window": 200000,
		"self_evolve":    false,
		"skill_evolve":   true,
		"emoji":          "🛠",
		"tools_config":   map[string]any{"allow": []any{"bash", "read_file"}},
		"reasoning_config": map[string]any{"effort": "medium"},
	})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	ag := fake.byKey["beta"]
	if ag.DisplayName != "Beta Agent" {
		t.Errorf("display_name mismatch: %s", ag.DisplayName)
	}
	if ag.Model != "opus" {
		t.Errorf("model mismatch: %s", ag.Model)
	}
	if ag.ContextWindow != 200000 {
		t.Errorf("context_window mismatch: %d", ag.ContextWindow)
	}
	if !ag.SkillEvolve {
		t.Errorf("skill_evolve should be true")
	}
	var tc map[string]any
	_ = json.Unmarshal(ag.ToolsConfig, &tc)
	allow, _ := tc["allow"].([]any)
	if len(allow) != 2 {
		t.Errorf("tools_config.allow mismatch: %v", allow)
	}
}

func TestAgentProvision_Create_WithContextFiles(t *testing.T) {
	tool, fake := mkProvisionTool(t)
	r := tool.Execute(context.Background(), map[string]any{
		"action":    "create",
		"agent_key": "gamma",
		"provider":  "claude-cli",
		"context_files": map[string]any{
			bootstrap.SoulFile:     "custom SOUL content",
			bootstrap.IdentityFile: "custom IDENTITY content",
		},
	})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	ag := fake.byKey["gamma"]
	soul := fake.files[ag.ID][bootstrap.SoulFile]
	if soul != "custom SOUL content" {
		t.Errorf("SOUL.md not overwritten with custom: %q", soul)
	}
	id := fake.files[ag.ID][bootstrap.IdentityFile]
	if id != "custom IDENTITY content" {
		t.Errorf("IDENTITY.md not overwritten with custom: %q", id)
	}
	// AGENTS.md should be present from seeding (default template).
	if _, ok := fake.files[ag.ID][bootstrap.AgentsFile]; !ok {
		t.Errorf("expected AGENTS.md to be seeded as default")
	}
}

func TestAgentProvision_Create_ContextFileBadName(t *testing.T) {
	tool, _ := mkProvisionTool(t)
	r := tool.Execute(context.Background(), map[string]any{
		"action": "create", "agent_key": "delta", "provider": "claude-cli",
		"context_files": map[string]any{"EVIL.md": "x"},
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "not an allowed context file") {
		t.Fatalf("expected allowlist error, got: %+v", r)
	}
}

func TestAgentProvision_Create_ContextFileTooLarge(t *testing.T) {
	tool, _ := mkProvisionTool(t)
	big := strings.Repeat("a", agentProvisionContextFileBytesMax+1)
	r := tool.Execute(context.Background(), map[string]any{
		"action": "create", "agent_key": "epsilon", "provider": "claude-cli",
		"context_files": map[string]any{bootstrap.SoulFile: big},
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "exceeds") {
		t.Fatalf("expected size error, got: %+v", r)
	}
}

func TestAgentProvision_Create_ContextFileNotString(t *testing.T) {
	tool, _ := mkProvisionTool(t)
	r := tool.Execute(context.Background(), map[string]any{
		"action": "create", "agent_key": "zeta", "provider": "claude-cli",
		"context_files": map[string]any{bootstrap.SoulFile: 42},
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "must be a string") {
		t.Fatalf("expected type error, got: %+v", r)
	}
}

func TestAgentProvision_Create_InvalidThinkingLevel(t *testing.T) {
	tool, _ := mkProvisionTool(t)
	r := tool.Execute(context.Background(), map[string]any{
		"action": "create", "agent_key": "alpha", "provider": "claude-cli",
		"thinking_level": "extreme",
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "thinking_level") {
		t.Fatalf("expected enum error, got: %+v", r)
	}
}

func TestAgentProvision_Create_StoreCreateError(t *testing.T) {
	tool, fake := mkProvisionTool(t)
	fake.createErr = errors.New("simulated DB failure")
	r := tool.Execute(context.Background(), map[string]any{
		"action": "create", "agent_key": "alpha", "provider": "claude-cli",
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "create failed") {
		t.Fatalf("expected create error, got: %+v", r)
	}
}

func TestAgentProvision_Create_DefaultWorkspace_NoWiring(t *testing.T) {
	// When SetDefaultWorkspace is not called, the tool should fall back to
	// "data/workspace" rather than empty.
	fake := newProvisionFake()
	tool := NewAgentProvisionTool()
	tool.SetAgentStore(fake)
	r := tool.Execute(context.Background(), map[string]any{
		"action": "create", "agent_key": "alpha", "provider": "claude-cli",
	})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	ag := fake.byKey["alpha"]
	if ag.Workspace != "data/workspace/alpha" {
		t.Errorf("unexpected fallback workspace: %s", ag.Workspace)
	}
}

func TestAgentProvision_Create_ExplicitWorkspace(t *testing.T) {
	tool, fake := mkProvisionTool(t)
	r := tool.Execute(context.Background(), map[string]any{
		"action": "create", "agent_key": "alpha", "provider": "claude-cli",
		"workspace": "/custom/path/alpha",
	})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	if fake.byKey["alpha"].Workspace != "/custom/path/alpha" {
		t.Errorf("explicit workspace not applied: %s", fake.byKey["alpha"].Workspace)
	}
}

// --- delete ---

func TestAgentProvision_Delete_MissingAgentID(t *testing.T) {
	tool, _ := mkProvisionTool(t)
	r := tool.Execute(context.Background(), map[string]any{"action": "delete"})
	if !r.IsError || !strings.Contains(r.ForLLM, "agent_id is required") {
		t.Fatalf("expected agent_id error, got: %+v", r)
	}
}

func TestAgentProvision_Delete_MissingConfirm(t *testing.T) {
	tool, fake := mkProvisionTool(t)
	_ = fake.Create(context.Background(), &store.AgentData{AgentKey: "alpha"})
	r := tool.Execute(context.Background(), map[string]any{
		"action": "delete", "agent_id": "alpha",
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "confirm=true") {
		t.Fatalf("expected confirm-required error, got: %+v", r)
	}
	// Agent should still exist.
	if _, ok := fake.byKey["alpha"]; !ok {
		t.Errorf("agent should not be deleted without confirm")
	}
}

func TestAgentProvision_Delete_FalseConfirm(t *testing.T) {
	tool, fake := mkProvisionTool(t)
	_ = fake.Create(context.Background(), &store.AgentData{AgentKey: "alpha"})
	r := tool.Execute(context.Background(), map[string]any{
		"action": "delete", "agent_id": "alpha", "confirm": false,
	})
	if !r.IsError {
		t.Fatalf("expected error for confirm=false")
	}
	if _, ok := fake.byKey["alpha"]; !ok {
		t.Errorf("agent should not be deleted with confirm=false")
	}
}

func TestAgentProvision_Delete_AgentNotFound(t *testing.T) {
	tool, _ := mkProvisionTool(t)
	r := tool.Execute(context.Background(), map[string]any{
		"action": "delete", "agent_id": "ghost", "confirm": true,
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "agent not found") {
		t.Fatalf("expected not-found, got: %+v", r)
	}
}

func TestAgentProvision_Delete_OK(t *testing.T) {
	tool, fake := mkProvisionTool(t)
	_ = fake.Create(context.Background(), &store.AgentData{AgentKey: "alpha"})
	r := tool.Execute(context.Background(), map[string]any{
		"action": "delete", "agent_id": "alpha", "confirm": true,
	})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	if _, ok := fake.byKey["alpha"]; ok {
		t.Error("agent should be deleted")
	}
	var resp map[string]any
	_ = json.Unmarshal([]byte(r.ForLLM), &resp)
	if resp["deleted"] != true {
		t.Errorf("expected deleted=true in response, got: %v", resp["deleted"])
	}
}

func TestAgentProvision_Delete_StoreError(t *testing.T) {
	tool, fake := mkProvisionTool(t)
	_ = fake.Create(context.Background(), &store.AgentData{AgentKey: "alpha"})
	fake.deleteErr = errors.New("simulated DB failure")
	r := tool.Execute(context.Background(), map[string]any{
		"action": "delete", "agent_id": "alpha", "confirm": true,
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "delete failed") {
		t.Fatalf("expected delete error, got: %+v", r)
	}
}

// --- list ---

func TestAgentProvision_List_Empty(t *testing.T) {
	tool, _ := mkProvisionTool(t)
	r := tool.Execute(context.Background(), map[string]any{"action": "list"})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	var resp map[string]any
	_ = json.Unmarshal([]byte(r.ForLLM), &resp)
	if resp["count"].(float64) != 0 {
		t.Errorf("expected count=0 for empty store, got: %v", resp["count"])
	}
}

func TestAgentProvision_List_WithAgents(t *testing.T) {
	tool, fake := mkProvisionTool(t)
	_ = fake.Create(context.Background(), &store.AgentData{
		AgentKey: "alpha", DisplayName: "Alpha", Provider: "claude-cli", Model: "opus",
		AgentType: store.AgentTypePredefined, Status: store.AgentStatusActive,
	})
	_ = fake.Create(context.Background(), &store.AgentData{
		AgentKey: "beta", DisplayName: "Beta", Provider: "openai", Model: "gpt-4",
		AgentType: store.AgentTypePredefined, Status: store.AgentStatusActive,
	})
	r := tool.Execute(context.Background(), map[string]any{"action": "list"})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	var resp map[string]any
	_ = json.Unmarshal([]byte(r.ForLLM), &resp)
	if resp["count"].(float64) != 2 {
		t.Errorf("expected count=2, got: %v", resp["count"])
	}
	agents, _ := resp["agents"].([]any)
	if len(agents) != 2 {
		t.Errorf("expected 2 agents in list, got: %d", len(agents))
	}
}

// --- end-to-end round-trip ---

func TestAgentProvision_CreateAndDelete_RoundTrip(t *testing.T) {
	tool, fake := mkProvisionTool(t)

	cr := tool.Execute(context.Background(), map[string]any{
		"action": "create", "agent_key": "alpha", "provider": "claude-cli",
	})
	if cr.IsError {
		t.Fatalf("create failed: %s", cr.ForLLM)
	}

	ls := tool.Execute(context.Background(), map[string]any{"action": "list"})
	var lsResp map[string]any
	_ = json.Unmarshal([]byte(ls.ForLLM), &lsResp)
	if lsResp["count"].(float64) != 1 {
		t.Errorf("expected 1 agent after create, got: %v", lsResp["count"])
	}

	dl := tool.Execute(context.Background(), map[string]any{
		"action": "delete", "agent_id": "alpha", "confirm": true,
	})
	if dl.IsError {
		t.Fatalf("delete failed: %s", dl.ForLLM)
	}

	ls2 := tool.Execute(context.Background(), map[string]any{"action": "list"})
	var ls2Resp map[string]any
	_ = json.Unmarshal([]byte(ls2.ForLLM), &ls2Resp)
	if ls2Resp["count"].(float64) != 0 {
		t.Errorf("expected 0 agents after delete, got: %v", ls2Resp["count"])
	}

	if len(fake.deleted) != 1 {
		t.Errorf("expected 1 delete recorded, got: %d", len(fake.deleted))
	}
}
