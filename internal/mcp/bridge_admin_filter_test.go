package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// fakeAgentLookup is a minimal stub of BridgeAgentLookup. Returns the agent
// keyed by id, or an error when err is set.
type fakeAgentLookup struct {
	agents map[uuid.UUID]*store.AgentData
	err    error
}

func (f *fakeAgentLookup) GetByIDUnscoped(_ context.Context, id uuid.UUID) (*store.AgentData, error) {
	if f.err != nil {
		return nil, f.err
	}
	ag, ok := f.agents[id]
	if !ok {
		return nil, nil
	}
	return ag, nil
}

func mustToolsConfig(t *testing.T, allow []string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"allow": allow})
	if err != nil {
		t.Fatalf("marshal tools_config: %v", err)
	}
	return raw
}

// allTools verifies the union of public + admin sets exhausts what the bridge
// could ever expose. Catches typos that move a name into both sets.
func TestBridgeToolSets_NoOverlap(t *testing.T) {
	for name := range BridgeToolNamesPublic {
		if BridgeToolNamesAdminOptIn[name] {
			t.Errorf("tool %q is in both Public and AdminOptIn sets", name)
		}
	}
}

func TestAgentAllowed_NilLookup_PublicOnly(t *testing.T) {
	got := agentAllowedBridgeTools(context.Background(), nil)
	if len(got) != len(BridgeToolNamesPublic) {
		t.Fatalf("expected %d public tools, got %d", len(BridgeToolNamesPublic), len(got))
	}
	for name := range BridgeToolNamesAdminOptIn {
		if got[name] {
			t.Errorf("admin tool %q must not be exposed when lookup is nil", name)
		}
	}
}

func TestAgentAllowed_NoAgentInCtx_PublicOnly(t *testing.T) {
	lookup := &fakeAgentLookup{agents: map[uuid.UUID]*store.AgentData{}}
	got := agentAllowedBridgeTools(context.Background(), lookup)
	for name := range BridgeToolNamesAdminOptIn {
		if got[name] {
			t.Errorf("admin tool %q must not be exposed when ctx has no agent id", name)
		}
	}
}

func TestAgentAllowed_AgentMissing_PublicOnly(t *testing.T) {
	id := uuid.New()
	lookup := &fakeAgentLookup{agents: map[uuid.UUID]*store.AgentData{}}
	ctx := store.WithAgentID(context.Background(), id)
	got := agentAllowedBridgeTools(ctx, lookup)
	for name := range BridgeToolNamesAdminOptIn {
		if got[name] {
			t.Errorf("admin tool %q must not be exposed when agent lookup misses", name)
		}
	}
}

func TestAgentAllowed_LookupError_PublicOnly(t *testing.T) {
	id := uuid.New()
	lookup := &fakeAgentLookup{err: errors.New("db down")}
	ctx := store.WithAgentID(context.Background(), id)
	got := agentAllowedBridgeTools(ctx, lookup)
	if len(got) != len(BridgeToolNamesPublic) {
		t.Fatalf("expected %d public tools on error, got %d", len(BridgeToolNamesPublic), len(got))
	}
}

func TestAgentAllowed_NoToolsConfig_PublicOnly(t *testing.T) {
	id := uuid.New()
	lookup := &fakeAgentLookup{
		agents: map[uuid.UUID]*store.AgentData{
			id: {AgentKey: "noop"}, // no ToolsConfig
		},
	}
	ctx := store.WithAgentID(context.Background(), id)
	got := agentAllowedBridgeTools(ctx, lookup)
	for name := range BridgeToolNamesAdminOptIn {
		if got[name] {
			t.Errorf("admin tool %q must not be exposed when agent has no tools_config", name)
		}
	}
}

func TestAgentAllowed_EmptyAllow_PublicOnly(t *testing.T) {
	id := uuid.New()
	lookup := &fakeAgentLookup{
		agents: map[uuid.UUID]*store.AgentData{
			id: {AgentKey: "lawyer", ToolsConfig: mustToolsConfig(t, []string{})},
		},
	}
	ctx := store.WithAgentID(context.Background(), id)
	got := agentAllowedBridgeTools(ctx, lookup)
	for name := range BridgeToolNamesAdminOptIn {
		if got[name] {
			t.Errorf("admin tool %q must not be exposed for agent with empty allow", name)
		}
	}
}

func TestAgentAllowed_AdminSubset(t *testing.T) {
	id := uuid.New()
	lookup := &fakeAgentLookup{
		agents: map[uuid.UUID]*store.AgentData{
			id: {
				AgentKey: "forge",
				ToolsConfig: mustToolsConfig(t, []string{
					"agent_provision", "agent_grants",
					// not admin → ignored by admin overlay (public ones come automatically)
					"read_file",
				}),
			},
		},
	}
	ctx := store.WithAgentID(context.Background(), id)
	got := agentAllowedBridgeTools(ctx, lookup)
	if !got["agent_provision"] {
		t.Error("expected agent_provision to be allowed for forge")
	}
	if !got["agent_grants"] {
		t.Error("expected agent_grants to be allowed for forge")
	}
	for name := range BridgeToolNamesAdminOptIn {
		if name == "agent_provision" || name == "agent_grants" {
			continue
		}
		if got[name] {
			t.Errorf("admin tool %q must not be exposed when not in allow", name)
		}
	}
	// Public tools always present.
	if !got["read_file"] || !got["web_search"] {
		t.Error("expected public tools to remain exposed")
	}
}

func TestAgentAllowed_AllAdmin(t *testing.T) {
	id := uuid.New()
	allow := make([]string, 0, len(BridgeToolNamesAdminOptIn))
	for name := range BridgeToolNamesAdminOptIn {
		allow = append(allow, name)
	}
	lookup := &fakeAgentLookup{
		agents: map[uuid.UUID]*store.AgentData{
			id: {AgentKey: "forge", ToolsConfig: mustToolsConfig(t, allow)},
		},
	}
	ctx := store.WithAgentID(context.Background(), id)
	got := agentAllowedBridgeTools(ctx, lookup)
	for name := range BridgeToolNamesAdminOptIn {
		if !got[name] {
			t.Errorf("admin tool %q must be exposed when listed in allow", name)
		}
	}
}

// Non-existent names in allow are ignored — only registered admin tools count.
func TestAgentAllowed_UnknownNamesInAllow_Ignored(t *testing.T) {
	id := uuid.New()
	lookup := &fakeAgentLookup{
		agents: map[uuid.UUID]*store.AgentData{
			id: {
				AgentKey:    "forge",
				ToolsConfig: mustToolsConfig(t, []string{"agent_provision", "totally_made_up_tool"}),
			},
		},
	}
	ctx := store.WithAgentID(context.Background(), id)
	got := agentAllowedBridgeTools(ctx, lookup)
	if !got["agent_provision"] {
		t.Error("expected agent_provision allowed")
	}
	if got["totally_made_up_tool"] {
		t.Error("unknown allow entries must not appear in result")
	}
}
