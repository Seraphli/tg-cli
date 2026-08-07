package stores

import (
	"encoding/json"
	"strings"
	"testing"
)

// Fix 20: RecordPending sets the ✍ receipt-ack reaction (add on receive); ClearReactions removes it
// (clear once CC starts processing). The set payload carries the ✍ emoji; the clear payload carries
// an empty (non-nil) reaction list which Telegram reads as "remove all reactions".
func TestBuildReactionPayloadSetAndClear(t *testing.T) {
	setB, _ := json.Marshal(buildReactionPayload(42, 100, "✍"))
	if !strings.Contains(string(setB), `"emoji":"✍"`) {
		t.Errorf("set payload must carry the ✍ emoji, got: %s", setB)
	}
	if !strings.Contains(string(setB), `"type":"emoji"`) {
		t.Errorf("set payload must carry type=emoji, got: %s", setB)
	}

	clearB, _ := json.Marshal(buildReactionPayload(42, 100))
	// Must serialize the reaction list as [] (remove-all), NEVER null (nil-slice bug would break the API).
	if !strings.Contains(string(clearB), `"reaction":[]`) {
		t.Errorf("clear payload must serialize reaction as [] (not null), got: %s", clearB)
	}
}

// Fix 20: the store keeps a single map of messages currently showing ✍. RecordPending tracks a
// message; ClearReactions drops all tracked messages for the target.
func TestReactionStoreRecordThenClear(t *testing.T) {
	rt := NewReactionTrackerStore()
	target := "%1@/tmp/tmux-1000/default"
	// nil bot → setReaction is a no-op (no network), state management still runs.
	rt.RecordPending(nil, target, 42, 100)
	rt.RecordPending(nil, target, 42, 101)
	if got := len(rt.showing[target]); got != 2 {
		t.Fatalf("expected 2 tracked reactions after two RecordPending, got %d", got)
	}
	rt.ClearReactions(nil, target)
	if got := len(rt.showing[target]); got != 0 {
		t.Errorf("expected 0 tracked reactions after ClearReactions, got %d", got)
	}
}

// B.7: TakeReactions (STATE — runs on the Hook FIFO) removes and returns the tracked entries; ApplyClear
// (pure I/O — runs on the Message FIFO) does the setReaction clear for each. TakeReactions must drop the
// records immediately (before any I/O) and hand the captured entries to the caller for the enqueued clear.
func TestReactionStoreTakeAndApplyClear(t *testing.T) {
	rt := NewReactionTrackerStore()
	target := "%1@/tmp/tmux-1000/default"
	rt.RecordPending(nil, target, 42, 100)
	rt.RecordPending(nil, target, 42, 101)
	entries := rt.TakeReactions(target)
	if got := len(entries); got != 2 {
		t.Fatalf("TakeReactions must return the 2 tracked entries, got %d", got)
	}
	// State is dropped immediately by TakeReactions, before ApplyClear runs any I/O.
	if got := len(rt.showing[target]); got != 0 {
		t.Fatalf("TakeReactions must remove the tracked entries, got %d remaining", got)
	}
	// A second TakeReactions returns nothing (already taken) — the clear is exactly-once.
	if got := len(rt.TakeReactions(target)); got != 0 {
		t.Fatalf("second TakeReactions must return 0 entries, got %d", got)
	}
	// ApplyClear on the captured entries is a no-op with a nil bot (no network) but must not panic.
	rt.ApplyClear(nil, entries)
}
