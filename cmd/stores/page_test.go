package stores

import (
	"testing"
)

// TestPageEntry_RoundtripExtendedFields verifies the extended PageEntry fields
// (CLICommand, AgentName, Backend, ContextUsed*) are preserved through Store → Get.
// This guards against regression of Bug 1 where pagination callbacks rebuilt
// NotificationData without these fields, causing context% to reset to 0% on page turn.
func TestPageEntry_RoundtripExtendedFields(t *testing.T) {
	store := NewPageCacheStore()
	entry := &PageEntry{
		Chunks:            []string{"page1", "page2", "page3"},
		Event:             "ToolUse",
		Project:           "test-proj",
		CWD:               "/tmp/test",
		TmuxTarget:        "%42",
		ChatID:            12345,
		CLICommand:        "claude --model sonnet",
		AgentName:         "worker",
		Backend:           "cc",
		ContextUsedPct:    42,
		ContextUsedTokens: 84000,
		ContextWindowSize: 200000,
	}
	store.Store(101, "sess-abc", entry)
	got, ok := store.Get(101)
	if !ok {
		t.Fatal("entry not found after Store")
	}
	if got.CLICommand != "claude --model sonnet" {
		t.Errorf("CLICommand: got %q, want %q", got.CLICommand, "claude --model sonnet")
	}
	if got.AgentName != "worker" {
		t.Errorf("AgentName: got %q, want %q", got.AgentName, "worker")
	}
	if got.Backend != "cc" {
		t.Errorf("Backend: got %q, want %q", got.Backend, "cc")
	}
	if got.ContextUsedPct != 42 {
		t.Errorf("ContextUsedPct: got %d, want 42", got.ContextUsedPct)
	}
	if got.ContextUsedTokens != 84000 {
		t.Errorf("ContextUsedTokens: got %d, want 84000", got.ContextUsedTokens)
	}
	if got.ContextWindowSize != 200000 {
		t.Errorf("ContextWindowSize: got %d, want 200000", got.ContextWindowSize)
	}
	if len(got.Chunks) != 3 || got.Chunks[1] != "page2" {
		t.Errorf("Chunks mismatch: %v", got.Chunks)
	}
}
