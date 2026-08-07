package stores

import (
	"strings"
	"testing"
)

// @ channel header format mirrors helpers.BuildAtHeader ("🔗 [@] `from` → `to`"). Hardcoded here to
// keep the stores package test free of a helpers import.
const (
	tcFwdHeader   = "🔗 [@] `e2e-cli` → `e2e-at-b`" // forward initiator→target
	tcReplyHeader = "🔗 [@] `e2e-at-b` → `e2e-cli`" // reply target→initiator
	tcOtherPair   = "🔗 [@] `e2e-cli` → `e2e-at-c`" // a DIFFERENT channel — must survive purge
)

// TestPurgeMatching_RemovesOnlyMatchingItems verifies PurgeMatching drops queued items whose text
// contains any provided marker, leaves non-matching items (plain injects AND other @ channel pairs)
// intact, and returns the correct removed count.
func TestPurgeMatching_RemovesOnlyMatchingItems(t *testing.T) {
	iq := NewInjectQueueStore(t.TempDir())
	pane := "%29@/tmp/tmux-1000/test"

	iq.Enqueue(pane, InjectItem{Text: tcFwdHeader + "\n---\nforward content"})
	iq.Enqueue(pane, InjectItem{Text: "unrelated injected prompt (not an @ message)"})
	iq.Enqueue(pane, InjectItem{Text: tcReplyHeader + "\n---\nreply content"})
	iq.Enqueue(pane, InjectItem{Text: tcOtherPair + "\n---\nother channel content"})

	if got := iq.ItemCount(pane); got != 4 {
		t.Fatalf("setup: expected 4 queued items, got %d", got)
	}

	removed := iq.PurgeMatching(pane, tcFwdHeader, tcReplyHeader)
	if removed != 2 {
		t.Fatalf("expected 2 items removed, got %d", removed)
	}

	texts := iq.GetTexts(pane)
	if len(texts) != 2 {
		t.Fatalf("expected 2 items remaining, got %d: %v", len(texts), texts)
	}
	for _, tx := range texts {
		if strings.Contains(tx, tcFwdHeader) || strings.Contains(tx, tcReplyHeader) {
			t.Fatalf("a matching item survived the purge: %q", tx)
		}
	}
	foundUnrelated, foundOther := false, false
	for _, tx := range texts {
		if strings.Contains(tx, "unrelated injected prompt") {
			foundUnrelated = true
		}
		if strings.Contains(tx, tcOtherPair) {
			foundOther = true
		}
	}
	if !foundUnrelated {
		t.Error("the unrelated (non-@) item was wrongly purged")
	}
	if !foundOther {
		t.Error("the other @ channel pair item was wrongly purged")
	}
}

// TestPurgeMatching_NoMatchLeavesQueueIntact verifies a purge with no matching markers removes
// nothing and returns 0.
func TestPurgeMatching_NoMatchLeavesQueueIntact(t *testing.T) {
	iq := NewInjectQueueStore(t.TempDir())
	pane := "%1@/tmp/tmux/test"
	iq.Enqueue(pane, InjectItem{Text: "some text"})
	iq.Enqueue(pane, InjectItem{Text: "another text"})
	if removed := iq.PurgeMatching(pane, "NONEXISTENT-MARKER"); removed != 0 {
		t.Fatalf("expected 0 removed, got %d", removed)
	}
	if iq.ItemCount(pane) != 2 {
		t.Fatalf("expected queue intact (2 items), got %d", iq.ItemCount(pane))
	}
}

// TestPurgeMatching_RemovingAllClearsQueue verifies purging every item deletes the target's queue
// entry so HasItems reports false.
func TestPurgeMatching_RemovingAllClearsQueue(t *testing.T) {
	iq := NewInjectQueueStore(t.TempDir())
	pane := "%2@/tmp/tmux/test"
	marker := "🔗 [@] `a` → `b`"
	iq.Enqueue(pane, InjectItem{Text: marker + "\n---\nx"})
	iq.Enqueue(pane, InjectItem{Text: marker + "\n---\ny"})
	if removed := iq.PurgeMatching(pane, marker); removed != 2 {
		t.Fatalf("expected 2 removed, got %d", removed)
	}
	if iq.HasItems(pane) {
		t.Fatal("expected queue empty after purging all items")
	}
}

// TestPurgeMatching_EmptyTargetReturnsZero verifies purging a target with no queue returns 0.
func TestPurgeMatching_EmptyTargetReturnsZero(t *testing.T) {
	iq := NewInjectQueueStore(t.TempDir())
	if removed := iq.PurgeMatching("%nonexistent", "anything"); removed != 0 {
		t.Fatalf("expected 0 removed for empty target, got %d", removed)
	}
}

// TestPurgeMatching_EmptyMarkerIgnored verifies an empty marker string never matches (so a purge
// with only an empty marker is a no-op), guarding against accidentally clearing the whole queue.
func TestPurgeMatching_EmptyMarkerIgnored(t *testing.T) {
	iq := NewInjectQueueStore(t.TempDir())
	pane := "%3@/tmp/tmux/test"
	iq.Enqueue(pane, InjectItem{Text: "keep me"})
	if removed := iq.PurgeMatching(pane, ""); removed != 0 {
		t.Fatalf("expected empty marker to match nothing, got %d removed", removed)
	}
	if iq.ItemCount(pane) != 1 {
		t.Fatalf("expected item retained, got %d", iq.ItemCount(pane))
	}
}
