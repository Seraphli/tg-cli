package cmd

import (
	"strings"
	"testing"

	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/cmd/types"
)

// TestPostStop_UnarmedEntryNotRendered (commit 18 seam): a genuinely-new post-Stop entry that has been stored
// but NOT YET armed (register.go resolves chat between storage and arm) must be ABSENT from the flush render
// snapshot — a concurrent ticker flush must render nothing and enqueue no chat=0 op. Once armed, its content
// renders.
func TestPostStop_UnarmedEntryNotRendered(t *testing.T) {
	var bodies []string
	bot, closeFn := newFakeStopBot(t, &bodies)
	defer closeFn()
	bs := &types.BotState{Bot: bot, Streams: stores.NewStreamStore(), MessageQueue: stores.NewSessionEventStore(), MsgIDMap: stores.NewMsgIDMap()}
	sid := "s-unarmed"

	// Prime + Stop (renders/seals the Stop bubble).
	bs.Streams.AppendDelta(sid, stores.StreamMeta{MessageID: "m0", ChatID: 1}, 0, "stop body", true)
	bs.Streams.FinalizeLastWithText(sid, "stop body")
	bs.Streams.MarkStopped(sid)
	streamFlush(bs, sid, true)
	sendsAfterStop := len(bodies)

	// A genuinely-new post-Stop delta arrives and requests arming, but is NOT yet armed.
	if _, _, post := bs.Streams.AppendExisting(sid, "mNew", "T1", 0, "brand new content", false); post != stores.PostStopNeedsArm {
		t.Fatalf("want PostStopNeedsArm, got %v", post)
	}
	// A concurrent flush at this instant (flushSync = deterministic) must render the UNARMED entry nowhere.
	flushSession(bs, sid, false, false, flushSync)
	if len(bodies) != sendsAfterStop {
		t.Fatalf("unarmed post-stop entry was rendered (want no new sends): before=%d after=%d new=%v",
			sendsAfterStop, len(bodies), bodies[sendsAfterStop:])
	}

	// Arm it (as register.go does after ResolveChat) → its content now renders.
	if armed, _ := bs.Streams.ArmStopped(sid, "mNew", stores.StreamMeta{MessageID: "mNew", ChatID: 1}); !armed {
		t.Fatal("ArmStopped should arm mNew")
	}
	flushSession(bs, sid, false, false, flushSync)
	if !strings.Contains(strings.Join(bodies, "\n"), "brand new content") {
		t.Fatalf("armed post-stop entry was not rendered; sent=%v", bodies)
	}
}

// TestPostStop_ABRelabel_PlaceholderNotTarget (commit 18 seam): with a genuine armed bubble A and an unarmed
// post-Stop placeholder B, the Stop flush relabels/closes A (last in ss.Order) and NEVER targets B — B is never
// in ss.Order.
func TestPostStop_ABRelabel_PlaceholderNotTarget(t *testing.T) {
	var bodies []string
	bot, closeFn := newFakeStopBot(t, &bodies)
	defer closeFn()
	bs := &types.BotState{Bot: bot, Streams: stores.NewStreamStore(), MessageQueue: stores.NewSessionEventStore(), MsgIDMap: stores.NewMsgIDMap()}
	sid := "s-ab"

	// Genuine bubble A completes; Stop installs the authoritative text (== A) and marks Stopped.
	bs.Streams.AppendDelta(sid, stores.StreamMeta{MessageID: "A", ChatID: 1}, 0, "bubble A content", true)
	bs.Streams.FinalizeLastWithText(sid, "bubble A content")
	bs.Streams.MarkStopped(sid)

	// An unarmed post-Stop placeholder B exists (all-empty here — never classifies, never arms).
	if _, _, post := bs.Streams.AppendExisting(sid, "B", "T1", 0, "", false); post != stores.PostStopDefer {
		t.Fatalf("B placeholder: want PostStopDefer, got %v", post)
	}

	// Stop flush: relabels the last Order entry (A), closes the turn.
	streamFlush(bs, sid, true)

	if !strings.Contains(strings.Join(bodies, "\n"), "bubble A content") {
		t.Fatalf("genuine bubble A must be rendered/relabeled; sent=%v", bodies)
	}
	ss := bs.Streams.Session(sid)
	ss.DataMu.Lock()
	bInOrder := false
	for _, id := range ss.Order {
		if id == "B" {
			bInOrder = true
		}
	}
	aRelabeled := ss.Msgs["A"] != nil && ss.Msgs["A"].Relabeled
	ss.DataMu.Unlock()
	if bInOrder {
		t.Fatal("placeholder B must never be in ss.Order (never a relabel target)")
	}
	if !aRelabeled {
		t.Fatal("genuine bubble A must be the relabel target")
	}
}
