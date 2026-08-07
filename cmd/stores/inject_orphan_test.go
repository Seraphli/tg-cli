package stores

import (
	"testing"
)

// TestInjectQueue_ClearTarget verifies ClearTarget drops all three maps for the target,
// leaves sibling targets intact, and persists correctly (reload shows target absent).
func TestInjectQueue_ClearTarget(t *testing.T) {
	dir := t.TempDir()
	iq := NewInjectQueueStore(dir)

	targetA := "%10@/tmp/tmux-1000/test"
	targetB := "%11@/tmp/tmux-1000/test"

	// Set up target A with 2 items + notify msg
	iq.Enqueue(targetA, InjectItem{Text: "item-a-1"})
	iq.Enqueue(targetA, InjectItem{Text: "item-a-2"})
	// SetNotifyMsg requires queued items (guard introduced in 3c)
	iq.SetNotifyMsg(targetA, 42)

	// Set up target B with 1 item + notify msg
	iq.Enqueue(targetB, InjectItem{Text: "item-b-1"})
	iq.SetNotifyMsg(targetB, 99)

	// ClearTarget(A) must return 2 (items dropped)
	if n := iq.ClearTarget(targetA); n != 2 {
		t.Fatalf("ClearTarget(A) expected 2, got %d", n)
	}

	// A is gone
	if iq.ItemCount(targetA) != 0 {
		t.Errorf("ItemCount(A) expected 0 after ClearTarget")
	}
	if iq.HasItems(targetA) {
		t.Errorf("HasItems(A) expected false after ClearTarget")
	}
	if _, ok := iq.GetNotifyMsg(targetA); ok {
		t.Errorf("GetNotifyMsg(A) expected not-found after ClearTarget")
	}
	if iq.GetInjectID(targetA) != "" {
		t.Errorf("GetInjectID(A) expected empty after ClearTarget")
	}

	// B is intact
	if iq.ItemCount(targetB) == 0 {
		t.Errorf("ItemCount(B) expected >0 — B must survive ClearTarget(A)")
	}
	if _, ok := iq.GetNotifyMsg(targetB); !ok {
		t.Errorf("GetNotifyMsg(B) expected found — B must survive ClearTarget(A)")
	}
	if iq.GetInjectID(targetB) == "" {
		t.Errorf("GetInjectID(B) expected non-empty — B must survive ClearTarget(A)")
	}

	// Reload from same dir: A absent, B present
	iq2 := NewInjectQueueStore(dir)
	iq2.Load()
	if iq2.ItemCount(targetA) != 0 {
		t.Errorf("after reload: ItemCount(A) expected 0, got %d", iq2.ItemCount(targetA))
	}
	if iq2.ItemCount(targetB) == 0 {
		t.Errorf("after reload: ItemCount(B) expected >0")
	}

	// ClearTarget on unknown/empty target returns 0
	if n := iq.ClearTarget("%99@/tmp/tmux/nope"); n != 0 {
		t.Errorf("ClearTarget on unknown target expected 0, got %d", n)
	}
}

// TestInjectQueue_ClearDeadTargets verifies ClearDeadTargets reaps only dead targets.
func TestInjectQueue_ClearDeadTargets(t *testing.T) {
	dir := t.TempDir()
	iq := NewInjectQueueStore(dir)

	dead := "%20@/tmp/tmux-1000/test"
	live := "%21@/tmp/tmux-1000/test"

	iq.Enqueue(dead, InjectItem{Text: "dead-1"})
	iq.Enqueue(dead, InjectItem{Text: "dead-2"})
	iq.Enqueue(live, InjectItem{Text: "live-1"})

	sessionExists := func(target string) bool {
		return target == live
	}

	total := iq.ClearDeadTargets(sessionExists)
	if total != 2 {
		t.Fatalf("ClearDeadTargets expected 2 items cleared, got %d", total)
	}
	if iq.HasItems(dead) {
		t.Errorf("dead target must have no items after ClearDeadTargets")
	}
	if !iq.HasItems(live) {
		t.Errorf("live target must still have items after ClearDeadTargets")
	}
}
