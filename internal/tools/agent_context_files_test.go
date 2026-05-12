package tools

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/bootstrap"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// ctxFilesFake is an in-memory AgentStore for agent_context_files tests.
// It embeds stubAgentStore (from context_file_interceptor_test.go) to
// inherit no-op implementations of all 30+ AgentStore methods, then
// overrides the 4 we actually exercise.
type ctxFilesFake struct {
	*stubAgentStore
	mu         sync.Mutex
	byKey      map[string]*store.AgentData
	byID       map[uuid.UUID]*store.AgentData
	files      map[uuid.UUID]map[string]string
	failNextDB bool
}

func newCtxFilesFake() *ctxFilesFake {
	return &ctxFilesFake{
		stubAgentStore: &stubAgentStore{},
		byKey:          make(map[string]*store.AgentData),
		byID:           make(map[uuid.UUID]*store.AgentData),
		files:          make(map[uuid.UUID]map[string]string),
	}
}

func (f *ctxFilesFake) addAgent(key string) *store.AgentData {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := uuid.New()
	ag := &store.AgentData{AgentKey: key}
	ag.ID = id
	f.byKey[key] = ag
	f.byID[id] = ag
	f.files[id] = make(map[string]string)
	return ag
}

func (f *ctxFilesFake) setFile(agentID uuid.UUID, name, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.files[agentID]; !ok {
		f.files[agentID] = make(map[string]string)
	}
	f.files[agentID][name] = content
}

func (f *ctxFilesFake) GetByKey(_ context.Context, key string) (*store.AgentData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ag, ok := f.byKey[key]; ok {
		return ag, nil
	}
	return nil, errors.New("agent not found: " + key)
}

func (f *ctxFilesFake) GetByID(_ context.Context, id uuid.UUID) (*store.AgentData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ag, ok := f.byID[id]; ok {
		return ag, nil
	}
	return nil, errors.New("agent not found: " + id.String())
}

func (f *ctxFilesFake) GetAgentContextFiles(_ context.Context, agentID uuid.UUID) ([]store.AgentContextFileData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNextDB {
		f.failNextDB = false
		return nil, errors.New("forced DB failure")
	}
	out := []store.AgentContextFileData{}
	for name, content := range f.files[agentID] {
		out = append(out, store.AgentContextFileData{AgentID: agentID, FileName: name, Content: content})
	}
	return out, nil
}

func (f *ctxFilesFake) SetAgentContextFile(_ context.Context, agentID uuid.UUID, fileName, content string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNextDB {
		f.failNextDB = false
		return errors.New("forced DB failure")
	}
	if _, ok := f.files[agentID]; !ok {
		f.files[agentID] = make(map[string]string)
	}
	f.files[agentID][fileName] = content
	return nil
}

func (f *ctxFilesFake) DeleteAgentContextFile(_ context.Context, agentID uuid.UUID, fileName string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNextDB {
		f.failNextDB = false
		return false, errors.New("forced DB failure")
	}
	bucket, ok := f.files[agentID]
	if !ok {
		return false, nil
	}
	if _, exists := bucket[fileName]; !exists {
		return false, nil
	}
	delete(bucket, fileName)
	return true, nil
}

func mkTool(t *testing.T) (*AgentContextFilesTool, *ctxFilesFake) {
	t.Helper()
	fake := newCtxFilesFake()
	tool := NewAgentContextFilesTool()
	tool.SetAgentStore(fake)
	return tool, fake
}

func ctxBg() context.Context { return context.Background() }

// --- dispatch + validation ---

func TestAgentContextFiles_NoStore(t *testing.T) {
	tool := NewAgentContextFilesTool()
	r := tool.Execute(ctxBg(), map[string]any{"action": "list", "agent_id": "x"})
	if !r.IsError || !strings.Contains(r.ForLLM, "agent store not wired") {
		t.Fatalf("expected store-not-wired error, got: %+v", r)
	}
}

func TestAgentContextFiles_MissingAction(t *testing.T) {
	tool, _ := mkTool(t)
	r := tool.Execute(ctxBg(), map[string]any{})
	if !r.IsError || !strings.Contains(r.ForLLM, "action parameter is required") {
		t.Fatalf("expected missing-action error, got: %+v", r)
	}
}

func TestAgentContextFiles_UnknownAction(t *testing.T) {
	tool, _ := mkTool(t)
	r := tool.Execute(ctxBg(), map[string]any{"action": "frobnicate"})
	if !r.IsError || !strings.Contains(r.ForLLM, "unknown action") {
		t.Fatalf("expected unknown-action error, got: %+v", r)
	}
}

// --- list ---

func TestAgentContextFiles_List_AgentNotFound(t *testing.T) {
	tool, _ := mkTool(t)
	r := tool.Execute(ctxBg(), map[string]any{"action": "list", "agent_id": "ghost"})
	if !r.IsError || !strings.Contains(r.ForLLM, "agent not found") {
		t.Fatalf("expected agent-not-found, got: %+v", r)
	}
}

func TestAgentContextFiles_List_EmptyAgent(t *testing.T) {
	tool, fake := mkTool(t)
	fake.addAgent("alpha")
	r := tool.Execute(ctxBg(), map[string]any{"action": "list", "agent_id": "alpha"})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	if !strings.Contains(r.ForLLM, "agent: alpha") {
		t.Fatalf("expected agent header, got: %s", r.ForLLM)
	}
	// All allowed files should be reported (as missing)
	for _, f := range allowedContextFiles {
		if !strings.Contains(r.ForLLM, f) {
			t.Errorf("expected file %q in listing, got: %s", f, r.ForLLM)
		}
	}
}

func TestAgentContextFiles_List_MixedPresence(t *testing.T) {
	tool, fake := mkTool(t)
	ag := fake.addAgent("alpha")
	fake.setFile(ag.ID, bootstrap.SoulFile, "soul body")
	fake.setFile(ag.ID, bootstrap.IdentityFile, "id body")
	r := tool.Execute(ctxBg(), map[string]any{"action": "list", "agent_id": "alpha"})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	if !strings.Contains(r.ForLLM, "SOUL.md") || !strings.Contains(r.ForLLM, "present") {
		t.Fatalf("expected SOUL.md present marker, got: %s", r.ForLLM)
	}
	if !strings.Contains(r.ForLLM, "CAPABILITIES.md") || !strings.Contains(r.ForLLM, "missing") {
		t.Fatalf("expected CAPABILITIES.md missing marker, got: %s", r.ForLLM)
	}
}

// --- read ---

func TestAgentContextFiles_Read_BadFileName(t *testing.T) {
	tool, fake := mkTool(t)
	fake.addAgent("alpha")
	r := tool.Execute(ctxBg(), map[string]any{"action": "read", "agent_id": "alpha", "file_name": "EVIL.md"})
	if !r.IsError || !strings.Contains(r.ForLLM, "not in allowlist") {
		t.Fatalf("expected allowlist error, got: %+v", r)
	}
}

func TestAgentContextFiles_Read_MissingFileName(t *testing.T) {
	tool, fake := mkTool(t)
	fake.addAgent("alpha")
	r := tool.Execute(ctxBg(), map[string]any{"action": "read", "agent_id": "alpha"})
	if !r.IsError || !strings.Contains(r.ForLLM, "file_name is required") {
		t.Fatalf("expected file_name-required error, got: %+v", r)
	}
}

func TestAgentContextFiles_Read_NotPresent(t *testing.T) {
	tool, fake := mkTool(t)
	fake.addAgent("alpha")
	r := tool.Execute(ctxBg(), map[string]any{"action": "read", "agent_id": "alpha", "file_name": bootstrap.SoulFile})
	if !r.IsError || !strings.Contains(r.ForLLM, "not found") {
		t.Fatalf("expected not-found, got: %+v", r)
	}
}

func TestAgentContextFiles_Read_OK(t *testing.T) {
	tool, fake := mkTool(t)
	ag := fake.addAgent("alpha")
	fake.setFile(ag.ID, bootstrap.SoulFile, "hello world")
	r := tool.Execute(ctxBg(), map[string]any{"action": "read", "agent_id": "alpha", "file_name": bootstrap.SoulFile})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	if !strings.Contains(r.ForLLM, "hello world") || !strings.Contains(r.ForLLM, "size=11") {
		t.Fatalf("expected content + size, got: %s", r.ForLLM)
	}
}

func TestAgentContextFiles_Read_ByUUID(t *testing.T) {
	tool, fake := mkTool(t)
	ag := fake.addAgent("alpha")
	fake.setFile(ag.ID, bootstrap.SoulFile, "via uuid")
	r := tool.Execute(ctxBg(), map[string]any{"action": "read", "agent_id": ag.ID.String(), "file_name": bootstrap.SoulFile})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	if !strings.Contains(r.ForLLM, "via uuid") {
		t.Fatalf("expected content, got: %s", r.ForLLM)
	}
}

// --- write ---

func TestAgentContextFiles_Write_BadFileName(t *testing.T) {
	tool, fake := mkTool(t)
	fake.addAgent("alpha")
	r := tool.Execute(ctxBg(), map[string]any{"action": "write", "agent_id": "alpha", "file_name": "EVIL.md", "content": "x"})
	if !r.IsError || !strings.Contains(r.ForLLM, "not in allowlist") {
		t.Fatalf("expected allowlist error, got: %+v", r)
	}
}

func TestAgentContextFiles_Write_NoContentField(t *testing.T) {
	tool, fake := mkTool(t)
	fake.addAgent("alpha")
	r := tool.Execute(ctxBg(), map[string]any{"action": "write", "agent_id": "alpha", "file_name": bootstrap.SoulFile})
	if !r.IsError || !strings.Contains(r.ForLLM, "requires content field") {
		t.Fatalf("expected content-required error, got: %+v", r)
	}
}

func TestAgentContextFiles_Write_TooLarge(t *testing.T) {
	tool, fake := mkTool(t)
	fake.addAgent("alpha")
	big := strings.Repeat("a", agentContextFilesMaxContentBytes+1)
	r := tool.Execute(ctxBg(), map[string]any{"action": "write", "agent_id": "alpha", "file_name": bootstrap.SoulFile, "content": big})
	if !r.IsError || !strings.Contains(r.ForLLM, "exceeds") {
		t.Fatalf("expected size error, got: %+v", r)
	}
}

func TestAgentContextFiles_Write_Created(t *testing.T) {
	tool, fake := mkTool(t)
	fake.addAgent("alpha")
	r := tool.Execute(ctxBg(), map[string]any{"action": "write", "agent_id": "alpha", "file_name": bootstrap.SoulFile, "content": "fresh"})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	if !strings.Contains(r.ForLLM, "write created") {
		t.Fatalf("expected 'write created', got: %s", r.ForLLM)
	}
	if !strings.Contains(r.ForLLM, "fresh") {
		t.Fatalf("expected post-state content, got: %s", r.ForLLM)
	}
	if strings.Contains(r.ForLLM, "previous_size=") {
		t.Errorf("created result should not include previous_size, got: %s", r.ForLLM)
	}
}

func TestAgentContextFiles_Write_Updated(t *testing.T) {
	tool, fake := mkTool(t)
	ag := fake.addAgent("alpha")
	fake.setFile(ag.ID, bootstrap.SoulFile, "old version")
	r := tool.Execute(ctxBg(), map[string]any{"action": "write", "agent_id": "alpha", "file_name": bootstrap.SoulFile, "content": "new version"})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	if !strings.Contains(r.ForLLM, "write updated") {
		t.Fatalf("expected 'write updated', got: %s", r.ForLLM)
	}
	if !strings.Contains(r.ForLLM, "previous_size=11") {
		t.Fatalf("expected previous_size=11, got: %s", r.ForLLM)
	}
	if !strings.Contains(r.ForLLM, "new version") {
		t.Fatalf("expected new content in response, got: %s", r.ForLLM)
	}
}

func TestAgentContextFiles_Write_EmptyAllowed(t *testing.T) {
	tool, fake := mkTool(t)
	fake.addAgent("alpha")
	r := tool.Execute(ctxBg(), map[string]any{"action": "write", "agent_id": "alpha", "file_name": bootstrap.BootstrapFile, "content": ""})
	if r.IsError {
		t.Fatalf("unexpected error for empty write: %s", r.ForLLM)
	}
}

func TestAgentContextFiles_Write_CapsLargeResponse(t *testing.T) {
	tool, fake := mkTool(t)
	fake.addAgent("alpha")
	body := strings.Repeat("z", agentContextFilesResponseCapBytes+1024)
	r := tool.Execute(ctxBg(), map[string]any{"action": "write", "agent_id": "alpha", "file_name": bootstrap.SoulFile, "content": body})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	if !strings.Contains(r.ForLLM, "truncated") {
		t.Fatalf("expected truncation marker, got first 200 chars: %s", r.ForLLM[:min(200, len(r.ForLLM))])
	}
}

func TestAgentContextFiles_Write_StoreFailure(t *testing.T) {
	tool, fake := mkTool(t)
	fake.addAgent("alpha")
	fake.failNextDB = true
	r := tool.Execute(ctxBg(), map[string]any{"action": "write", "agent_id": "alpha", "file_name": bootstrap.SoulFile, "content": "x"})
	if !r.IsError {
		t.Fatalf("expected error on store failure, got: %+v", r)
	}
}

// --- delete ---

func TestAgentContextFiles_Delete_Protected(t *testing.T) {
	protected := []string{
		bootstrap.SoulFile, bootstrap.IdentityFile, bootstrap.CapabilitiesFile,
		bootstrap.UserPredefinedFile, bootstrap.AgentsFile, bootstrap.HeartbeatFile,
	}
	for _, name := range protected {
		t.Run(name, func(t *testing.T) {
			tool, fake := mkTool(t)
			fake.addAgent("alpha")
			r := tool.Execute(ctxBg(), map[string]any{"action": "delete", "agent_id": "alpha", "file_name": name})
			if !r.IsError || !strings.Contains(r.ForLLM, "protected from deletion") {
				t.Fatalf("expected protection error for %s, got: %+v", name, r)
			}
		})
	}
}

func TestAgentContextFiles_Delete_NotPresent(t *testing.T) {
	tool, fake := mkTool(t)
	fake.addAgent("alpha")
	r := tool.Execute(ctxBg(), map[string]any{"action": "delete", "agent_id": "alpha", "file_name": bootstrap.BootstrapFile})
	if r.IsError {
		t.Fatalf("unexpected error (should be idempotent noop): %s", r.ForLLM)
	}
	if !strings.Contains(r.ForLLM, "was not present") {
		t.Fatalf("expected noop message, got: %s", r.ForLLM)
	}
}

func TestAgentContextFiles_Delete_OK(t *testing.T) {
	tool, fake := mkTool(t)
	ag := fake.addAgent("alpha")
	fake.setFile(ag.ID, bootstrap.BootstrapFile, "hello bootstrap")
	r := tool.Execute(ctxBg(), map[string]any{"action": "delete", "agent_id": "alpha", "file_name": bootstrap.BootstrapFile})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	if !strings.Contains(r.ForLLM, "delete ok") || !strings.Contains(r.ForLLM, "previous_size=15") {
		t.Fatalf("expected delete-ok with previous_size, got: %s", r.ForLLM)
	}
	if _, ok := fake.files[ag.ID][bootstrap.BootstrapFile]; ok {
		t.Fatal("file should have been deleted from underlying store")
	}
}

func TestAgentContextFiles_Delete_BadFileName(t *testing.T) {
	tool, fake := mkTool(t)
	fake.addAgent("alpha")
	r := tool.Execute(ctxBg(), map[string]any{"action": "delete", "agent_id": "alpha", "file_name": "RANDOM.md"})
	if !r.IsError || !strings.Contains(r.ForLLM, "not in allowlist") {
		t.Fatalf("expected allowlist error, got: %+v", r)
	}
}
