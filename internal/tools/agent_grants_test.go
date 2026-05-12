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

// grantsAgentFake is a tiny in-memory AgentStore for agent_grants tests.
// Embeds stubAgentStore for the rest of the interface.
type grantsAgentFake struct {
	*stubAgentStore
	mu    sync.Mutex
	byKey map[string]*store.AgentData
	byID  map[uuid.UUID]*store.AgentData
}

func newGrantsAgentFake() *grantsAgentFake {
	return &grantsAgentFake{
		stubAgentStore: &stubAgentStore{},
		byKey:          make(map[string]*store.AgentData),
		byID:           make(map[uuid.UUID]*store.AgentData),
	}
}

func (f *grantsAgentFake) addAgent(key string) *store.AgentData {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := uuid.New()
	ag := &store.AgentData{AgentKey: key}
	ag.ID = id
	f.byKey[key] = ag
	f.byID[id] = ag
	return ag
}

func (f *grantsAgentFake) GetByKey(_ context.Context, key string) (*store.AgentData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ag, ok := f.byKey[key]; ok {
		return ag, nil
	}
	return nil, errors.New("agent not found: " + key)
}

func (f *grantsAgentFake) GetByID(_ context.Context, id uuid.UUID) (*store.AgentData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ag, ok := f.byID[id]; ok {
		return ag, nil
	}
	return nil, errors.New("agent not found")
}

// --- skill store fake ---

type fakeSkillStore struct {
	mu             sync.Mutex
	skills         []store.SkillInfo
	grants         map[string]map[uuid.UUID]int // skillUUID → agentID → version
	bumpCount      int
	grantErr       error
	revokeErr      error
	listGrantsErr  error
}

func newFakeSkillStore() *fakeSkillStore {
	return &fakeSkillStore{
		grants: make(map[string]map[uuid.UUID]int),
	}
}

func (f *fakeSkillStore) addSkill(id uuid.UUID, slug, name string, version int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.skills = append(f.skills, store.SkillInfo{
		ID:          id.String(),
		Slug:        slug,
		Name:        name,
		Description: "test " + slug,
		Version:     version,
		Visibility:  "internal",
	})
}

func (f *fakeSkillStore) ListSkills(_ context.Context) []store.SkillInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.SkillInfo, len(f.skills))
	copy(out, f.skills)
	return out
}

func (f *fakeSkillStore) GetSkillByID(_ context.Context, id uuid.UUID) (store.SkillInfo, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.skills {
		if s.ID == id.String() {
			return s, true
		}
	}
	return store.SkillInfo{}, false
}

func (f *fakeSkillStore) GrantToAgent(_ context.Context, skillID, agentID uuid.UUID, version int, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.grantErr != nil {
		err := f.grantErr
		f.grantErr = nil
		return err
	}
	if _, ok := f.grants[skillID.String()]; !ok {
		f.grants[skillID.String()] = make(map[uuid.UUID]int)
	}
	f.grants[skillID.String()][agentID] = version
	return nil
}

func (f *fakeSkillStore) RevokeFromAgent(_ context.Context, skillID, agentID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.revokeErr != nil {
		err := f.revokeErr
		f.revokeErr = nil
		return err
	}
	if m, ok := f.grants[skillID.String()]; ok {
		delete(m, agentID)
	}
	return nil
}

func (f *fakeSkillStore) ListWithGrantStatus(_ context.Context, agentID uuid.UUID) ([]store.SkillWithGrantStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listGrantsErr != nil {
		err := f.listGrantsErr
		f.listGrantsErr = nil
		return nil, err
	}
	out := make([]store.SkillWithGrantStatus, 0, len(f.skills))
	for _, s := range f.skills {
		sUUID, _ := uuid.Parse(s.ID)
		granted := false
		if m, ok := f.grants[s.ID]; ok {
			_, granted = m[agentID]
		}
		out = append(out, store.SkillWithGrantStatus{
			ID:          sUUID,
			Name:        s.Name,
			Slug:        s.Slug,
			Description: s.Description,
			Visibility:  s.Visibility,
			Version:     s.Version,
			Granted:     granted,
			IsSystem:    s.IsSystem,
		})
	}
	return out, nil
}

func (f *fakeSkillStore) BumpVersion() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bumpCount++
}

// --- mcp store fake ---

type fakeMCPStore struct {
	mu         sync.Mutex
	servers    map[uuid.UUID]*store.MCPServerData
	byName     map[string]*store.MCPServerData
	grants     map[uuid.UUID][]store.MCPAgentGrant // agentID → grants
	grantErr   error
	revokeErr  error
}

func newFakeMCPStore() *fakeMCPStore {
	return &fakeMCPStore{
		servers: make(map[uuid.UUID]*store.MCPServerData),
		byName:  make(map[string]*store.MCPServerData),
		grants:  make(map[uuid.UUID][]store.MCPAgentGrant),
	}
}

func (f *fakeMCPStore) addServer(name string) *store.MCPServerData {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := &store.MCPServerData{
		Name:      name,
		Transport: "stdio",
		Enabled:   true,
	}
	s.ID = uuid.New()
	f.servers[s.ID] = s
	f.byName[name] = s
	return s
}

func (f *fakeMCPStore) ListServers(_ context.Context) ([]store.MCPServerData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.MCPServerData, 0, len(f.servers))
	for _, s := range f.servers {
		out = append(out, *s)
	}
	return out, nil
}

func (f *fakeMCPStore) GetServer(_ context.Context, id uuid.UUID) (*store.MCPServerData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.servers[id]; ok {
		cp := *s
		return &cp, nil
	}
	return nil, errors.New("not found")
}

func (f *fakeMCPStore) GetServerByName(_ context.Context, name string) (*store.MCPServerData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.byName[name]; ok {
		cp := *s
		return &cp, nil
	}
	return nil, errors.New("not found")
}

func (f *fakeMCPStore) GrantToAgent(_ context.Context, g *store.MCPAgentGrant) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.grantErr != nil {
		err := f.grantErr
		f.grantErr = nil
		return err
	}
	g.ID = uuid.New()
	f.grants[g.AgentID] = append(f.grants[g.AgentID], *g)
	return nil
}

func (f *fakeMCPStore) RevokeFromAgent(_ context.Context, serverID, agentID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.revokeErr != nil {
		err := f.revokeErr
		f.revokeErr = nil
		return err
	}
	kept := f.grants[agentID][:0]
	for _, g := range f.grants[agentID] {
		if g.ServerID != serverID {
			kept = append(kept, g)
		}
	}
	f.grants[agentID] = kept
	return nil
}

func (f *fakeMCPStore) ListAgentGrants(_ context.Context, agentID uuid.UUID) ([]store.MCPAgentGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.MCPAgentGrant, len(f.grants[agentID]))
	copy(out, f.grants[agentID])
	return out, nil
}

// --- harness ---

func mkGrantsTool(t *testing.T) (*AgentGrantsTool, *grantsAgentFake, *fakeSkillStore, *fakeMCPStore) {
	t.Helper()
	a := newGrantsAgentFake()
	s := newFakeSkillStore()
	m := newFakeMCPStore()
	tool := NewAgentGrantsTool()
	tool.SetAgentStore(a)
	tool.SetSkillStore(s)
	tool.SetMCPStore(m)
	return tool, a, s, m
}

// --- dispatch ---

func TestAgentGrants_NoStore(t *testing.T) {
	tool := NewAgentGrantsTool()
	r := tool.Execute(context.Background(), map[string]any{"action": "list", "agent_id": "x"})
	if !r.IsError || !strings.Contains(r.ForLLM, "agent store not wired") {
		t.Fatalf("expected store error, got: %+v", r)
	}
}

func TestAgentGrants_MissingAction(t *testing.T) {
	tool, _, _, _ := mkGrantsTool(t)
	r := tool.Execute(context.Background(), map[string]any{})
	if !r.IsError || !strings.Contains(r.ForLLM, "action") {
		t.Fatalf("expected action error, got: %+v", r)
	}
}

func TestAgentGrants_UnknownAction(t *testing.T) {
	tool, _, _, _ := mkGrantsTool(t)
	r := tool.Execute(context.Background(), map[string]any{"action": "frobnicate"})
	if !r.IsError || !strings.Contains(r.ForLLM, "unknown action") {
		t.Fatalf("expected unknown-action, got: %+v", r)
	}
}

// --- list ---

func TestAgentGrants_List_AgentNotFound(t *testing.T) {
	tool, _, _, _ := mkGrantsTool(t)
	r := tool.Execute(context.Background(), map[string]any{"action": "list", "agent_id": "ghost"})
	if !r.IsError || !strings.Contains(r.ForLLM, "agent not found") {
		t.Fatalf("expected agent-not-found, got: %+v", r)
	}
}

func TestAgentGrants_List_Empty(t *testing.T) {
	tool, af, _, _ := mkGrantsTool(t)
	af.addAgent("alpha")
	r := tool.Execute(context.Background(), map[string]any{"action": "list", "agent_id": "alpha"})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	var resp map[string]any
	_ = json.Unmarshal([]byte(r.ForLLM), &resp)
	skills, _ := resp["skills"].([]any)
	if len(skills) != 0 {
		t.Errorf("expected 0 skills, got: %v", skills)
	}
	mcp, _ := resp["mcp"].([]any)
	if len(mcp) != 0 {
		t.Errorf("expected 0 mcp, got: %v", mcp)
	}
}

func TestAgentGrants_List_WithGrants(t *testing.T) {
	tool, af, sf, mf := mkGrantsTool(t)
	ag := af.addAgent("alpha")
	sk1 := uuid.New()
	sf.addSkill(sk1, "kg-ontology", "KG Ontology", 1)
	sf.addSkill(uuid.New(), "skill-packaging", "Skill Packaging", 2) // ungranted

	srv := mf.addServer("camoufox")

	// Pre-populate grants directly
	_ = sf.GrantToAgent(context.Background(), sk1, ag.ID, 1, "tester")
	_ = mf.GrantToAgent(context.Background(), &store.MCPAgentGrant{
		ServerID: srv.ID, AgentID: ag.ID, Enabled: true, GrantedBy: "tester",
	})

	r := tool.Execute(context.Background(), map[string]any{"action": "list", "agent_id": "alpha"})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	var resp map[string]any
	_ = json.Unmarshal([]byte(r.ForLLM), &resp)
	skills, _ := resp["skills"].([]any)
	if len(skills) != 1 {
		t.Fatalf("expected 1 granted skill, got: %v", skills)
	}
	mcp, _ := resp["mcp"].([]any)
	if len(mcp) != 1 {
		t.Fatalf("expected 1 mcp grant, got: %v", mcp)
	}
	mcpEntry, _ := mcp[0].(map[string]any)
	if mcpEntry["server_name"] != "camoufox" {
		t.Errorf("expected server_name=camoufox, got: %v", mcpEntry["server_name"])
	}
}

// --- list_*_available ---

func TestAgentGrants_ListSkillsAvailable(t *testing.T) {
	tool, _, sf, _ := mkGrantsTool(t)
	sf.addSkill(uuid.New(), "alpha", "Alpha", 1)
	sf.addSkill(uuid.New(), "beta", "Beta", 1)
	r := tool.Execute(context.Background(), map[string]any{"action": "list_skills_available"})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	var resp map[string]any
	_ = json.Unmarshal([]byte(r.ForLLM), &resp)
	if resp["count"].(float64) != 2 {
		t.Errorf("expected count=2, got: %v", resp["count"])
	}
}

func TestAgentGrants_ListMCPAvailable(t *testing.T) {
	tool, _, _, mf := mkGrantsTool(t)
	mf.addServer("alpha")
	mf.addServer("beta")
	r := tool.Execute(context.Background(), map[string]any{"action": "list_mcp_available"})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	var resp map[string]any
	_ = json.Unmarshal([]byte(r.ForLLM), &resp)
	if resp["count"].(float64) != 2 {
		t.Errorf("expected count=2, got: %v", resp["count"])
	}
}

// --- grant_skill ---

func TestAgentGrants_GrantSkill_MissingSkillID(t *testing.T) {
	tool, af, _, _ := mkGrantsTool(t)
	af.addAgent("alpha")
	r := tool.Execute(context.Background(), map[string]any{"action": "grant_skill", "agent_id": "alpha"})
	if !r.IsError || !strings.Contains(r.ForLLM, "skill_id is required") {
		t.Fatalf("expected skill_id required, got: %+v", r)
	}
}

func TestAgentGrants_GrantSkill_SkillNotFound(t *testing.T) {
	tool, af, _, _ := mkGrantsTool(t)
	af.addAgent("alpha")
	r := tool.Execute(context.Background(), map[string]any{
		"action": "grant_skill", "agent_id": "alpha", "skill_id": "nonexistent-slug",
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "skill not found") {
		t.Fatalf("expected skill-not-found, got: %+v", r)
	}
}

func TestAgentGrants_GrantSkill_BySlug(t *testing.T) {
	tool, af, sf, _ := mkGrantsTool(t)
	ag := af.addAgent("alpha")
	sk1 := uuid.New()
	sf.addSkill(sk1, "kg-ontology", "KG Ontology", 1)

	r := tool.Execute(context.Background(), map[string]any{
		"action": "grant_skill", "agent_id": "alpha", "skill_id": "kg-ontology",
	})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	if _, ok := sf.grants[sk1.String()][ag.ID]; !ok {
		t.Errorf("grant not recorded in store")
	}
	if sf.bumpCount != 1 {
		t.Errorf("expected BumpVersion called once, got: %d", sf.bumpCount)
	}
}

func TestAgentGrants_GrantSkill_ByUUID(t *testing.T) {
	tool, af, sf, _ := mkGrantsTool(t)
	ag := af.addAgent("alpha")
	sk1 := uuid.New()
	sf.addSkill(sk1, "kg-ontology", "KG Ontology", 1)

	r := tool.Execute(context.Background(), map[string]any{
		"action": "grant_skill", "agent_id": "alpha", "skill_id": sk1.String(), "version": 2,
	})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	if sf.grants[sk1.String()][ag.ID] != 2 {
		t.Errorf("expected version 2, got: %d", sf.grants[sk1.String()][ag.ID])
	}
}

func TestAgentGrants_GrantSkill_StoreError(t *testing.T) {
	tool, af, sf, _ := mkGrantsTool(t)
	af.addAgent("alpha")
	sk1 := uuid.New()
	sf.addSkill(sk1, "kg-ontology", "KG", 1)
	sf.grantErr = errors.New("simulated DB failure")

	r := tool.Execute(context.Background(), map[string]any{
		"action": "grant_skill", "agent_id": "alpha", "skill_id": "kg-ontology",
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "grant_skill failed") {
		t.Fatalf("expected store error, got: %+v", r)
	}
}

// --- revoke_skill ---

func TestAgentGrants_RevokeSkill_OK(t *testing.T) {
	tool, af, sf, _ := mkGrantsTool(t)
	ag := af.addAgent("alpha")
	sk1 := uuid.New()
	sf.addSkill(sk1, "kg-ontology", "KG", 1)
	_ = sf.GrantToAgent(context.Background(), sk1, ag.ID, 1, "tester")

	r := tool.Execute(context.Background(), map[string]any{
		"action": "revoke_skill", "agent_id": "alpha", "skill_id": "kg-ontology",
	})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	if _, ok := sf.grants[sk1.String()][ag.ID]; ok {
		t.Errorf("grant should be removed from store")
	}
}

func TestAgentGrants_RevokeSkill_NotFound(t *testing.T) {
	tool, af, _, _ := mkGrantsTool(t)
	af.addAgent("alpha")
	r := tool.Execute(context.Background(), map[string]any{
		"action": "revoke_skill", "agent_id": "alpha", "skill_id": "nonexistent",
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "skill not found") {
		t.Fatalf("expected skill-not-found, got: %+v", r)
	}
}

// --- grant_mcp ---

func TestAgentGrants_GrantMCP_MissingServerID(t *testing.T) {
	tool, af, _, _ := mkGrantsTool(t)
	af.addAgent("alpha")
	r := tool.Execute(context.Background(), map[string]any{"action": "grant_mcp", "agent_id": "alpha"})
	if !r.IsError || !strings.Contains(r.ForLLM, "server_id is required") {
		t.Fatalf("expected server_id required, got: %+v", r)
	}
}

func TestAgentGrants_GrantMCP_ServerNotFound(t *testing.T) {
	tool, af, _, _ := mkGrantsTool(t)
	af.addAgent("alpha")
	r := tool.Execute(context.Background(), map[string]any{
		"action": "grant_mcp", "agent_id": "alpha", "server_id": "ghost",
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "MCP server not found") {
		t.Fatalf("expected server-not-found, got: %+v", r)
	}
}

func TestAgentGrants_GrantMCP_ByName(t *testing.T) {
	tool, af, _, mf := mkGrantsTool(t)
	ag := af.addAgent("alpha")
	srv := mf.addServer("camoufox")

	r := tool.Execute(context.Background(), map[string]any{
		"action": "grant_mcp", "agent_id": "alpha", "server_id": "camoufox",
	})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	grants, _ := mf.ListAgentGrants(context.Background(), ag.ID)
	if len(grants) != 1 || grants[0].ServerID != srv.ID {
		t.Errorf("expected 1 grant for srv, got: %+v", grants)
	}
}

func TestAgentGrants_GrantMCP_WithToolAllow(t *testing.T) {
	tool, af, _, mf := mkGrantsTool(t)
	ag := af.addAgent("alpha")
	_ = mf.addServer("camoufox")

	r := tool.Execute(context.Background(), map[string]any{
		"action": "grant_mcp", "agent_id": "alpha", "server_id": "camoufox",
		"tool_allow": []any{"page_snapshot", "fill"},
		"tool_deny":  []any{"navigate"},
	})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	grants, _ := mf.ListAgentGrants(context.Background(), ag.ID)
	if len(grants) != 1 {
		t.Fatalf("expected 1 grant, got: %d", len(grants))
	}
	g := grants[0]
	if !strings.Contains(string(g.ToolAllow), "page_snapshot") {
		t.Errorf("tool_allow not stored: %s", g.ToolAllow)
	}
	if !strings.Contains(string(g.ToolDeny), "navigate") {
		t.Errorf("tool_deny not stored: %s", g.ToolDeny)
	}
}

func TestAgentGrants_GrantMCP_StoreError(t *testing.T) {
	tool, af, _, mf := mkGrantsTool(t)
	af.addAgent("alpha")
	_ = mf.addServer("camoufox")
	mf.grantErr = errors.New("simulated DB failure")

	r := tool.Execute(context.Background(), map[string]any{
		"action": "grant_mcp", "agent_id": "alpha", "server_id": "camoufox",
	})
	if !r.IsError || !strings.Contains(r.ForLLM, "grant_mcp failed") {
		t.Fatalf("expected store error, got: %+v", r)
	}
}

// --- revoke_mcp ---

func TestAgentGrants_RevokeMCP_OK(t *testing.T) {
	tool, af, _, mf := mkGrantsTool(t)
	ag := af.addAgent("alpha")
	srv := mf.addServer("camoufox")
	_ = mf.GrantToAgent(context.Background(), &store.MCPAgentGrant{ServerID: srv.ID, AgentID: ag.ID})

	r := tool.Execute(context.Background(), map[string]any{
		"action": "revoke_mcp", "agent_id": "alpha", "server_id": "camoufox",
	})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	grants, _ := mf.ListAgentGrants(context.Background(), ag.ID)
	if len(grants) != 0 {
		t.Errorf("expected 0 grants after revoke, got: %d", len(grants))
	}
}

// --- combined flow ---

func TestAgentGrants_GrantThenList(t *testing.T) {
	tool, af, sf, mf := mkGrantsTool(t)
	af.addAgent("alpha")
	sk1 := uuid.New()
	sf.addSkill(sk1, "kg-ontology", "KG", 1)
	_ = mf.addServer("camoufox")

	if r := tool.Execute(context.Background(), map[string]any{
		"action": "grant_skill", "agent_id": "alpha", "skill_id": "kg-ontology",
	}); r.IsError {
		t.Fatalf("grant_skill failed: %s", r.ForLLM)
	}
	if r := tool.Execute(context.Background(), map[string]any{
		"action": "grant_mcp", "agent_id": "alpha", "server_id": "camoufox",
	}); r.IsError {
		t.Fatalf("grant_mcp failed: %s", r.ForLLM)
	}

	r := tool.Execute(context.Background(), map[string]any{"action": "list", "agent_id": "alpha"})
	if r.IsError {
		t.Fatalf("list failed: %s", r.ForLLM)
	}
	var resp map[string]any
	_ = json.Unmarshal([]byte(r.ForLLM), &resp)
	skills, _ := resp["skills"].([]any)
	mcp, _ := resp["mcp"].([]any)
	if len(skills) != 1 {
		t.Errorf("expected 1 skill, got: %v", skills)
	}
	if len(mcp) != 1 {
		t.Errorf("expected 1 mcp grant, got: %v", mcp)
	}
}
