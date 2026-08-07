package stores

import "testing"

// TestMsgIDMap_AllocateSetGet (S14b): Allocate returns a fresh monotonic id; Set records the (tgMsgID,
// session) mapping; Get returns it. Mirrors the send-op path: allocate on the Hook FIFO, Set on the send op,
// Get on a later edit/result op.
func TestMsgIDMap_AllocateSetGet(t *testing.T) {
	m := NewMsgIDMap()
	id1 := m.Allocate()
	id2 := m.Allocate()
	if id1 == 0 || id2 == 0 || id1 == id2 {
		t.Fatalf("Allocate must return fresh non-zero monotonic ids, got %d and %d", id1, id2)
	}
	if id2 <= id1 {
		t.Fatalf("Allocate must be monotonically increasing, got %d then %d", id1, id2)
	}
	m.Set(id1, 555, "sessA")
	tg, ok := m.Get(id1)
	if !ok || tg != 555 {
		t.Fatalf("Get after Set must return the mapping, got tg=%d ok=%v", tg, ok)
	}
}

// TestMsgIDMap_GetMiss (S14b): a Get on an unmapped internal id returns !ok — the edit/result op logs+skips
// on this path (no recovery send, no panic). Assert the miss is reported cleanly.
func TestMsgIDMap_GetMiss(t *testing.T) {
	m := NewMsgIDMap()
	id := m.Allocate() // allocated but never Set (send failed / not yet delivered)
	if tg, ok := m.Get(id); ok || tg != 0 {
		t.Fatalf("Get on an unset id must miss, got tg=%d ok=%v", tg, ok)
	}
	if _, ok := m.Get(999999); ok {
		t.Fatal("Get on an unknown id must miss")
	}
}

// TestMsgIDMap_Delete (S14b): Delete removes a single mapping; a later Get misses (edit/result op then skips).
func TestMsgIDMap_Delete(t *testing.T) {
	m := NewMsgIDMap()
	id := m.Allocate()
	m.Set(id, 42, "sessA")
	m.Delete(id)
	if _, ok := m.Get(id); ok {
		t.Fatal("Delete must remove the mapping")
	}
}

// TestMsgIDMap_DeleteSession (S14b): DeleteSession removes ALL of a session's mappings (SessionEnd cleanup on
// the Message FIFO) while leaving other sessions untouched.
func TestMsgIDMap_DeleteSession(t *testing.T) {
	m := NewMsgIDMap()
	a1, a2, b1 := m.Allocate(), m.Allocate(), m.Allocate()
	m.Set(a1, 1, "sessA")
	m.Set(a2, 2, "sessA")
	m.Set(b1, 3, "sessB")
	m.DeleteSession("sessA")
	if _, ok := m.Get(a1); ok {
		t.Fatal("DeleteSession must remove sessA's first mapping")
	}
	if _, ok := m.Get(a2); ok {
		t.Fatal("DeleteSession must remove sessA's second mapping")
	}
	if tg, ok := m.Get(b1); !ok || tg != 3 {
		t.Fatalf("DeleteSession must NOT touch sessB, got tg=%d ok=%v", tg, ok)
	}
}

// TestMsgIDMap_OverflowOrderingInvariant (S14b): the compact-overflow op does Delete(old) then Set(new) in ONE
// op; after both, ONLY the new id is mapped (no resurrection of old, no leak). Simulate the single-op body.
func TestMsgIDMap_OverflowOrderingInvariant(t *testing.T) {
	m := NewMsgIDMap()
	oldID := m.Allocate()
	m.Set(oldID, 100, "sessA") // the prior compact message's mapping
	// The msg:compact-overflow op body: Delete(old) then Set(new) — ordered, in one op.
	newID := m.Allocate()
	m.Delete(oldID)
	m.Set(newID, 200, "sessA")
	if _, ok := m.Get(oldID); ok {
		t.Fatal("overflow op must delete the OLD mapping (no resurrection)")
	}
	if tg, ok := m.Get(newID); !ok || tg != 200 {
		t.Fatalf("overflow op must leave ONLY the new mapping, got tg=%d ok=%v", tg, ok)
	}
}
