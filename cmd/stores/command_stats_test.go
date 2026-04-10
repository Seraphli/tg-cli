package stores

import (
	"testing"
)

// TestRecord_IncrementsAndSetsDirty verifies that Record increments the counter and sets dirty flag.
func TestRecord_IncrementsAndSetsDirty(t *testing.T) {
	s := NewCommandStatsStore(t.TempDir())
	if s.IsDirty() {
		t.Fatal("expected dirty=false on new store")
	}
	s.Record("bot")
	s.Record("bot")
	s.Record("hook")
	if !s.IsDirty() {
		t.Fatal("expected dirty=true after Record")
	}
	all := s.GetAll()
	if all["bot"] != 2 {
		t.Errorf("bot count: got %d, want 2", all["bot"])
	}
	if all["hook"] != 1 {
		t.Errorf("hook count: got %d, want 1", all["hook"])
	}
}

// TestGetAll_ReturnsCopy verifies that mutating the returned map does not affect internal state.
func TestGetAll_ReturnsCopy(t *testing.T) {
	s := NewCommandStatsStore(t.TempDir())
	s.Record("bot")
	snap := s.GetAll()
	snap["bot"] = 999
	snap["extra"] = 42
	actual := s.GetAll()
	if actual["bot"] != 1 {
		t.Errorf("internal bot count altered: got %d, want 1", actual["bot"])
	}
	if _, ok := actual["extra"]; ok {
		t.Error("internal state gained extra key from mutated snapshot")
	}
}

// TestSaveLoad_Roundtrip verifies save+load preserves counts and clears dirty on save.
func TestSaveLoad_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	s := NewCommandStatsStore(dir)
	s.Record("bot")
	s.Record("bot")
	s.Record("voice")
	if err := s.SaveToDisk(); err != nil {
		t.Fatalf("SaveToDisk: %v", err)
	}
	if s.IsDirty() {
		t.Error("expected dirty=false after SaveToDisk")
	}
	s2 := NewCommandStatsStore(dir)
	if err := s2.LoadFromDisk(); err != nil {
		t.Fatalf("LoadFromDisk: %v", err)
	}
	all := s2.GetAll()
	if all["bot"] != 2 {
		t.Errorf("bot count after reload: got %d, want 2", all["bot"])
	}
	if all["voice"] != 1 {
		t.Errorf("voice count after reload: got %d, want 1", all["voice"])
	}
}

// TestLoadFromDisk_MissingFile verifies that a missing file is not an error and leaves counts empty.
func TestLoadFromDisk_MissingFile(t *testing.T) {
	s := NewCommandStatsStore(t.TempDir())
	if err := s.LoadFromDisk(); err != nil {
		t.Fatalf("expected nil error for missing file, got: %v", err)
	}
	if len(s.GetAll()) != 0 {
		t.Error("expected empty counts after LoadFromDisk with missing file")
	}
}
