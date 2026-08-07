package stores

import (
	"os"
	"testing"
	"time"
)

func TestBusyStatusStore_Reserve(t *testing.T) {
	dir := t.TempDir()
	s := NewBusyStatusStore(dir)

	// First Reserve creates a placeholder with MsgID==0.
	e, created := s.Reserve(100, 0)
	if !created {
		t.Fatal("first Reserve should create")
	}
	if e.MsgID != 0 {
		t.Errorf("placeholder MsgID = %d, want 0", e.MsgID)
	}
	if e.ChatID != 100 {
		t.Errorf("ChatID = %d, want 100", e.ChatID)
	}

	// Second Reserve returns the existing entry.
	_, created2 := s.Reserve(100, 0)
	if created2 {
		t.Fatal("second Reserve should NOT create (entry exists)")
	}
}

func TestBusyStatusStore_GetUpdateDelete(t *testing.T) {
	dir := t.TempDir()
	s := NewBusyStatusStore(dir)

	// Get on absent key returns false.
	_, ok := s.Get(1, 2)
	if ok {
		t.Fatal("Get on absent key should return false")
	}

	s.Reserve(1, 2)
	e, ok := s.Get(1, 2)
	if !ok {
		t.Fatal("Get after Reserve should find entry")
	}
	if e.MsgID != 0 {
		t.Errorf("initial MsgID = %d, want 0", e.MsgID)
	}

	// Update and read back.
	now := time.Now()
	e.MsgID = 42
	e.SentAt = now
	s.Update(e)

	e2, ok := s.Get(1, 2)
	if !ok || e2.MsgID != 42 {
		t.Errorf("Get after Update: got (MsgID=%d ok=%v), want (42 true)", e2.MsgID, ok)
	}

	// Delete and confirm absent.
	s.Delete(1, 2)
	_, ok = s.Get(1, 2)
	if ok {
		t.Fatal("Get after Delete should return false")
	}
}

func TestBusyStatusStore_GetAll(t *testing.T) {
	dir := t.TempDir()
	s := NewBusyStatusStore(dir)

	s.Reserve(1, 0)
	s.Reserve(2, 0)
	all := s.GetAll()
	if len(all) != 2 {
		t.Errorf("GetAll len = %d, want 2", len(all))
	}
}

func TestBusyStatusStore_Clear(t *testing.T) {
	dir := t.TempDir()
	s := NewBusyStatusStore(dir)

	s.Reserve(1, 0)
	s.Reserve(2, 0)
	s.Clear()
	if len(s.GetAll()) != 0 {
		t.Error("GetAll after Clear should return empty")
	}
}

func TestBusyStatusStore_PersistenceReload(t *testing.T) {
	dir := t.TempDir()
	s := NewBusyStatusStore(dir)

	s.Reserve(7, 3)
	e, _ := s.Get(7, 3)
	e.MsgID = 99
	s.Update(e)

	// Create a new store pointing at the same dir and load.
	s2 := NewBusyStatusStore(dir)
	s2.Load()

	e2, ok := s2.Get(7, 3)
	if !ok {
		t.Fatal("persistence: entry missing after reload")
	}
	if e2.MsgID != 99 {
		t.Errorf("persistence: MsgID = %d, want 99", e2.MsgID)
	}
}

func TestBusyStatusStore_TryBeginAction(t *testing.T) {
	dir := t.TempDir()
	s := NewBusyStatusStore(dir)

	key := "100:0"
	if !s.TryBeginAction(key) {
		t.Fatal("first TryBeginAction should succeed")
	}
	// While held, second attempt must fail.
	if s.TryBeginAction(key) {
		t.Fatal("second TryBeginAction while held should fail")
	}
	s.EndAction(key)
	// After EndAction, a new attempt must succeed.
	if !s.TryBeginAction(key) {
		t.Fatal("TryBeginAction after EndAction should succeed")
	}
	s.EndAction(key)
}

func TestBusyStatusStore_GetReturnsDefensiveCopy(t *testing.T) {
	dir := t.TempDir()
	s := NewBusyStatusStore(dir)

	s.Reserve(5, 0)
	e, _ := s.Get(5, 0)
	// Mutate the returned copy.
	e.MsgID = 999

	// The store must still have the original zero value.
	e2, _ := s.Get(5, 0)
	if e2.MsgID != 0 {
		t.Errorf("mutating returned copy changed the store (MsgID=%d)", e2.MsgID)
	}
}

func TestBusyStatusStore_GetAllReturnsDefensiveCopies(t *testing.T) {
	dir := t.TempDir()
	s := NewBusyStatusStore(dir)

	s.Reserve(10, 0)
	all := s.GetAll()
	all[0].MsgID = 777

	// The store must still have MsgID == 0.
	e, _ := s.Get(10, 0)
	if e.MsgID != 0 {
		t.Errorf("mutating GetAll copy changed the store (MsgID=%d)", e.MsgID)
	}
}

func TestBusyStatusStore_LoadMissingFile(t *testing.T) {
	dir := t.TempDir()
	s := NewBusyStatusStore(dir)
	// Load on a missing file must not panic or error.
	s.Load()
	os.Remove(dir + "/busy_status.json") // ensure file doesn't exist
	s.Load()
}
