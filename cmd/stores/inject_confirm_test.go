package stores

import "testing"

// TestInjectConfirmStore_NoEviction_DifferentSnippets verifies a second Register for the same target
// does not evict an earlier caller's still-waiting channel. notify (the fix) is synchronous — it locks,
// finds/removes the matching entry, unlocks, then sends on the buffered-1 channel, all before returning
// — so the channel can be read with a non-blocking select immediately after the Notify call returns.
func TestInjectConfirmStore_NoEviction_DifferentSnippets(t *testing.T) {
	store := NewInjectConfirmStore()
	ch1 := store.Register("t1", ConfirmUserPromptSubmit, "alpha")
	ch2 := store.Register("t1", ConfirmUserPromptSubmit, "beta")

	store.NotifyUserPromptSubmit("t1", "beta arrived")
	select {
	case v := <-ch2:
		if !v {
			t.Fatal("ch2: expected true, got false")
		}
	default:
		t.Fatal("ch2: expected a value after NotifyUserPromptSubmit, got none (entry evicted?)")
	}

	store.NotifyUserPromptSubmit("t1", "alpha arrived")
	select {
	case v := <-ch1:
		if !v {
			t.Fatal("ch1: expected true, got false")
		}
	default:
		t.Fatal("ch1: expected a value after NotifyUserPromptSubmit, got none (entry evicted by second Register?)")
	}
}

// TestInjectConfirmStore_FIFO_IdenticalSnippet verifies that when two entries share an identical
// snippet (mirroring the real phase25 %30 case), Notify resolves them oldest-first (FIFO).
func TestInjectConfirmStore_FIFO_IdenticalSnippet(t *testing.T) {
	store := NewInjectConfirmStore()
	snippet := "🔗 [@] `e2e-cli` → `e2e-at-b`"
	chOld := store.Register("t2", ConfirmUserPromptSubmit, snippet)
	chNew := store.Register("t2", ConfirmUserPromptSubmit, snippet)

	store.NotifyUserPromptSubmit("t2", snippet+" ... first prompt body")
	select {
	case v := <-chOld:
		if !v {
			t.Fatal("chOld: expected true, got false")
		}
	default:
		t.Fatal("chOld: expected a value after first NotifyUserPromptSubmit, got none")
	}
	select {
	case v := <-chNew:
		t.Fatalf("chNew: expected no value yet, got %v", v)
	default:
	}

	store.NotifyUserPromptSubmit("t2", snippet+" ... second prompt body")
	select {
	case v := <-chNew:
		if !v {
			t.Fatal("chNew: expected true, got false")
		}
	default:
		t.Fatal("chNew: expected a value after second NotifyUserPromptSubmit, got none")
	}
}

// TestInjectConfirmStore_CancelByChannel_DoesNotAffectSibling verifies Cancel removes only the entry
// owning the specified channel, leaving a sibling entry for the same target untouched.
func TestInjectConfirmStore_CancelByChannel_DoesNotAffectSibling(t *testing.T) {
	store := NewInjectConfirmStore()
	ch1 := store.Register("t3", ConfirmUserPromptSubmit, "one")
	ch2 := store.Register("t3", ConfirmUserPromptSubmit, "two")

	store.Cancel("t3", ch1)

	store.NotifyUserPromptSubmit("t3", "two arrived")
	select {
	case v := <-ch2:
		if !v {
			t.Fatal("ch2: expected true, got false")
		}
	default:
		t.Fatal("ch2: expected a value after NotifyUserPromptSubmit, got none")
	}
	select {
	case v := <-ch1:
		t.Fatalf("ch1: expected no value (cancelled entry), got %v", v)
	default:
	}
}

// TestInjectConfirmStore_CancelByChannel_RemovesOwningEntry verifies Cancel really removes the entry owning the
// specified channel: with "one" cancelled, a notification whose content matches "one" must not reach ch1 (it is
// content-mismatched against the only remaining entry, "two", which therefore gets false). A no-op Cancel would leave
// ch1 registered and deliver true to it.
func TestInjectConfirmStore_CancelByChannel_RemovesOwningEntry(t *testing.T) {
	store := NewInjectConfirmStore()
	ch1 := store.Register("t3", ConfirmUserPromptSubmit, "one")
	ch2 := store.Register("t3", ConfirmUserPromptSubmit, "two")

	store.Cancel("t3", ch1)

	store.NotifyUserPromptSubmit("t3", "one arrived")
	select {
	case v := <-ch1:
		t.Fatalf("ch1: expected no value (cancelled entry), got %v", v)
	default:
	}
	select {
	case v := <-ch2:
		if v {
			t.Fatal("ch2: expected false (content mismatch with the only remaining entry), got true")
		}
	default:
		t.Fatal("ch2: expected a value after NotifyUserPromptSubmit, got none")
	}
}
