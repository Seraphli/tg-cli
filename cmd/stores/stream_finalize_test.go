package stores

import (
	"testing"
	"time"
)

// lastText returns the last entry's assembled text + completeness + sealed flag (white-box).
func lastText(t *testing.T, s *StreamStore, sid string) (string, bool, bool) {
	t.Helper()
	ss := s.Session(sid)
	ss.DataMu.Lock()
	defer ss.DataMu.Unlock()
	if len(ss.Order) == 0 {
		return "", false, false
	}
	e := ss.Msgs[ss.Order[len(ss.Order)-1]]
	text, complete := e.AssembledText()
	return text, complete, e.Sealed
}

// D4(a): a partial (incomplete) last entry, once given the authoritative Stop text, becomes complete +
// sealed and assembles to the FULL text (not the partial fragment).
func TestFinalizeLastWithText_PartialToAuthoritative(t *testing.T) {
	s := NewStreamStore()
	sid := "s1"
	s.AppendDelta(sid, StreamMeta{MessageID: "m1", ChatID: 1}, 0, "partial fragment", false)
	if got := s.FinalizeLastWithText(sid, "the full authoritative reply"); got != FinalizeExisting {
		t.Fatalf("FinalizeLastWithText = %v, want FinalizeExisting", got)
	}
	text, complete, sealed := lastText(t, s, sid)
	if text != "the full authoritative reply" {
		t.Errorf("assembled text = %q, want the authoritative text", text)
	}
	if !complete {
		t.Errorf("entry should be complete after finalize")
	}
	if !sealed {
		t.Errorf("entry should be sealed (barrier) after finalize")
	}
}

// D4(b): after finalize, any late/out-of-order MD for the same (sealed) entry — including an index=0
// overwrite — is dropped, so the authoritative text cannot be altered.
func TestFinalizeLastWithText_LateMDDropped_Barrier(t *testing.T) {
	s := NewStreamStore()
	sid := "s1"
	s.AppendDelta(sid, StreamMeta{MessageID: "m1", ChatID: 1}, 0, "partial", false)
	s.FinalizeLastWithText(sid, "AUTHORITATIVE")

	// Late AppendExisting appending a new index → dropped (sealed).
	if _, dropped, _ := s.AppendExisting(sid, "m1", "", 1, " late tail", true); !dropped {
		t.Errorf("late AppendExisting (new index) should be dropped on a sealed entry")
	}
	// Late AppendExisting overwriting index 0 → dropped (sealed).
	if _, dropped, _ := s.AppendExisting(sid, "m1", "", 0, "OVERWRITE", true); !dropped {
		t.Errorf("late AppendExisting (index=0 overwrite) should be dropped on a sealed entry")
	}
	// Late AppendDelta for the same sealed message → not new, not applied.
	if isNew := s.AppendDelta(sid, StreamMeta{MessageID: "m1", ChatID: 1}, 0, "OVERWRITE2", true); isNew {
		t.Errorf("late AppendDelta on a sealed entry should not modify/create")
	}
	if text, _, _ := lastText(t, s, sid); text != "AUTHORITATIVE" {
		t.Errorf("authoritative text was altered by late MD: %q", text)
	}
}

// D4(c) part 1: Stop before any MD (empty order) reports FinalizeNoEntry with NO side effect — it must NOT
// mark the session Stopped (an instant stop would drop a slightly-late MD the grace should catch).
func TestFinalizeLastWithText_NoEntry_NoSideEffect(t *testing.T) {
	s := NewStreamStore()
	sid := "s1"
	if got := s.FinalizeLastWithText(sid, "PAYLOAD"); got != FinalizeNoEntry {
		t.Fatalf("FinalizeLastWithText = %v, want FinalizeNoEntry", got)
	}
	ss := s.Session(sid)
	ss.DataMu.Lock()
	stopped := ss.Stopped
	ss.DataMu.Unlock()
	if stopped {
		t.Errorf("FinalizeLastWithText(no-entry) must NOT mark the session Stopped")
	}
	// No side effect → a subsequent MD still creates the entry (the grace path can then catch it).
	if isNew := s.AppendDelta(sid, StreamMeta{MessageID: "m1", ChatID: 1}, 0, "md", false); !isNew {
		t.Errorf("MD after FinalizeNoEntry should create the entry (no stop was set)")
	}
}

// D4(c) part 2 — timeout branch: no MD within the grace → AwaitEntryOrStop returns false, atomically marks
// the session Stopped, and a later MD is then dropped (so the handler's direct-send stays a single message).
func TestAwaitEntryOrStop_Timeout_StopsAndDedups(t *testing.T) {
	s := NewStreamStore()
	sid := "s1"
	if s.AwaitEntryOrStop(sid, 40*time.Millisecond) {
		t.Fatalf("AwaitEntryOrStop should return false when no entry appears within the grace")
	}
	ss := s.Session(sid)
	ss.DataMu.Lock()
	stopped := ss.Stopped
	ss.DataMu.Unlock()
	if !stopped {
		t.Errorf("session should be marked Stopped after the grace times out")
	}
	if isNew := s.AppendDelta(sid, StreamMeta{MessageID: "m1", ChatID: 1}, 0, "late", false); isNew {
		t.Errorf("late AppendDelta after timeout-stop should be dropped (no duplicate)")
	}
	ss.DataMu.Lock()
	n := len(ss.Order)
	ss.DataMu.Unlock()
	if n != 0 {
		t.Errorf("no entry should exist after timeout-stop, got %d", n)
	}
}

// D4(c) part 3 — entry branch: a near-concurrent MD arriving during the grace makes AwaitEntryOrStop return
// true WITHOUT stopping the session. Synchronized via a goroutine (the generous grace deterministically
// catches the append — not timing luck).
func TestAwaitEntryOrStop_EntryArrivesWithinGrace(t *testing.T) {
	s := NewStreamStore()
	sid := "s1"
	done := make(chan struct{})
	go func() {
		s.AppendDelta(sid, StreamMeta{MessageID: "m1", ChatID: 1}, 0, "md within grace", true)
		close(done)
	}()
	if !s.AwaitEntryOrStop(sid, 2*time.Second) {
		t.Fatalf("AwaitEntryOrStop should return true when an entry arrives within the grace")
	}
	<-done
	ss := s.Session(sid)
	ss.DataMu.Lock()
	stopped := ss.Stopped
	n := len(ss.Order)
	ss.DataMu.Unlock()
	if stopped {
		t.Errorf("session must NOT be Stopped when an entry arrived within the grace")
	}
	if n != 1 {
		t.Errorf("entry should exist, got %d", n)
	}
}

// D4(d): with two entries, only the LAST is replaced with the authoritative text; earlier entries untouched.
func TestFinalizeLastWithText_TwoEntries_OnlyLastReplaced(t *testing.T) {
	s := NewStreamStore()
	sid := "s1"
	s.AppendDelta(sid, StreamMeta{MessageID: "m1", ChatID: 1}, 0, "first bubble", true)
	s.AppendDelta(sid, StreamMeta{MessageID: "m2", ChatID: 1}, 0, "second partial", false)
	if got := s.FinalizeLastWithText(sid, "SECOND AUTHORITATIVE"); got != FinalizeExisting {
		t.Fatalf("FinalizeLastWithText = %v, want FinalizeExisting", got)
	}
	ss := s.Session(sid)
	ss.DataMu.Lock()
	t1, _ := ss.Msgs["m1"].AssembledText()
	t2, _ := ss.Msgs["m2"].AssembledText()
	ss.DataMu.Unlock()
	if t1 != "first bubble" {
		t.Errorf("first entry must be untouched, got %q", t1)
	}
	if t2 != "SECOND AUTHORITATIVE" {
		t.Errorf("last entry must be replaced, got %q", t2)
	}
}

// TestFinalizeLastWithText_FourCases covers the four FinalizeResult outcomes in one deterministic sequence:
// (A) no entry -> FinalizeNoEntry, (B) unsealed entry -> FinalizeExisting, (C) sealed+matching -> FinalizeSkipped,
// (D) sealed+different text -> FinalizeSealedMismatch.
func TestFinalizeLastWithText_FourCases(t *testing.T) {
	s := NewStreamStore()
	sid := "s-four"

	// (A) fresh store, no entry: must return FinalizeNoEntry.
	if got := s.FinalizeLastWithText(sid, "hello"); got != FinalizeNoEntry {
		t.Fatalf("case A: FinalizeLastWithText = %v, want FinalizeNoEntry", got)
	}

	// (B) append one unsealed entry, then finalize with authoritative text: must return FinalizeExisting and seal.
	s.AppendDelta(sid, StreamMeta{MessageID: "m1", ChatID: 1}, 0, "partial", false)
	if got := s.FinalizeLastWithText(sid, "authoritative"); got != FinalizeExisting {
		t.Fatalf("case B: FinalizeLastWithText = %v, want FinalizeExisting", got)
	}
	text, complete, sealed := lastText(t, s, sid)
	if text != "authoritative" {
		t.Fatalf("case B: assembled text = %q, want %q", text, "authoritative")
	}
	if !complete {
		t.Fatalf("case B: entry must be complete after FinalizeExisting")
	}
	if !sealed {
		t.Fatalf("case B: entry must be sealed after FinalizeExisting")
	}

	// (C) call again with the same text on the now-sealed entry: assembled matches Stop text -> FinalizeSkipped.
	if got := s.FinalizeLastWithText(sid, "authoritative"); got != FinalizeSkipped {
		t.Fatalf("case C: FinalizeLastWithText = %v, want FinalizeSkipped (sealed entry, same text)", got)
	}

	// (D) call with DIFFERENT text on the sealed entry: assembled does NOT match -> FinalizeSealedMismatch.
	if got := s.FinalizeLastWithText(sid, "DOGS different"); got != FinalizeSealedMismatch {
		t.Fatalf("case D: FinalizeLastWithText = %v, want FinalizeSealedMismatch (sealed entry, different text)", got)
	}
}

// armedInOrder reports whether messageID is armed (present in ss.Order). White-box helper.
func armedInOrder(s *StreamStore, sid, messageID string) bool {
	ss := s.Session(sid)
	ss.DataMu.Lock()
	defer ss.DataMu.Unlock()
	return isInOrder(ss, messageID)
}

// TestAppendExisting_PostStop covers the post-Stop state machine returns (commit 18): a known-sealed id is a
// drop, a delta that verbatim-matches the Stop body is a sticky STOP-COPY drop (never armed), and a genuinely-
// new delta requests arming (NeedsArm), then streams (ArmedProgress) and completes (ArmedComplete) once armed.
func TestAppendExisting_PostStop(t *testing.T) {
	s := NewStreamStore()
	sid := "s-poststop"
	s.AppendDelta(sid, StreamMeta{MessageID: "m1", ChatID: 1}, 0, "hello", true)
	s.FinalizeLastWithText(sid, "hello")
	s.MarkStopped(sid)

	// (a) known, sealed id → safe drop (late/duplicate on the sealed Stop entry).
	if h, d, post := s.AppendExisting(sid, "m1", "", 1, " more", true); !h || !d || post != PostStopNone {
		t.Fatalf("(a) known-sealed post-stop: want handled=true dropped=true post=None, got %v %v %v", h, d, post)
	}
	// (b) new id, delta == Stop body → STOP-COPY drop (dedup, no double-send).
	if h, d, post := s.AppendExisting(sid, "m2copy", "", 0, "hello", true); !h || !d || post != PostStopDrop {
		t.Fatalf("(b) stop-copy post-stop: want handled=true dropped=true post=PostStopDrop, got %v %v %v", h, d, post)
	}
	if armedInOrder(s, sid, "m2copy") {
		t.Fatal("(b) STOP-COPY placeholder must never be armed (absent from ss.Order)")
	}
	// (b') a later delta of the STOP-COPY id is still dropped without re-matching (sticky, no storage).
	if h, d, post := s.AppendExisting(sid, "m2copy", "", 1, "BRAND NEW", true); !h || !d || post != PostStopDrop {
		t.Fatalf("(b') STOP-COPY sticky: want dropped + PostStopDrop, got %v %v %v", h, d, post)
	}
	// (c) new id, different content, non-final → genuinely-new message requests arming.
	if h, d, post := s.AppendExisting(sid, "m3new", "", 0, "part1", false); !h || d || post != PostStopNeedsArm {
		t.Fatalf("(c) genuinely-new post-stop: want handled=true dropped=false post=PostStopNeedsArm, got %v %v %v", h, d, post)
	}
	if armedInOrder(s, sid, "m3new") {
		t.Fatal("(c) NEW entry is NOT armed until the caller calls ArmStopped (absent from ss.Order)")
	}
	// (c') still unarmed → a continuation keeps asking to arm.
	if h, d, post := s.AppendExisting(sid, "m3new", "", 1, "part2", false); !h || d || post != PostStopNeedsArm {
		t.Fatalf("(c') unarmed NEW continuation: want post=PostStopNeedsArm, got %v %v %v", h, d, post)
	}
	// Arm it (installs metadata + Order). Not yet complete (no final delta).
	if armed, complete := s.ArmStopped(sid, "m3new", StreamMeta{MessageID: "m3new", ChatID: 1}); !armed || complete {
		t.Fatalf("ArmStopped m3new: want armed=true complete=false, got %v %v", armed, complete)
	}
	if !armedInOrder(s, sid, "m3new") {
		t.Fatal("m3new must be armed (present in ss.Order) after ArmStopped")
	}
	// (d) armed, non-final continuation → ArmedProgress (ticker renders it).
	if h, d, post := s.AppendExisting(sid, "m3new", "", 2, "part3", false); !h || d || post != PostStopArmedProgress {
		t.Fatalf("(d) armed progress: want post=PostStopArmedProgress, got %v %v %v", h, d, post)
	}
	// (e) armed, completing delta → ArmedComplete (completion boundary → caller flushes).
	if h, d, post := s.AppendExisting(sid, "m3new", "", 3, "END", true); !h || d || post != PostStopArmedComplete {
		t.Fatalf("(e) armed complete: want post=PostStopArmedComplete, got %v %v %v", h, d, post)
	}
}

// D3: empty/whitespace payload and an already-sealed last entry both return Skipped without overwriting.
func TestFinalizeLastWithText_Skipped(t *testing.T) {
	// Empty/whitespace payload → Skipped, entry unchanged.
	s := NewStreamStore()
	sid := "s1"
	s.AppendDelta(sid, StreamMeta{MessageID: "m1", ChatID: 1}, 0, "partial", false)
	if got := s.FinalizeLastWithText(sid, "   "); got != FinalizeSkipped {
		t.Errorf("whitespace payload = %v, want FinalizeSkipped", got)
	}
	if text, _, _ := lastText(t, s, sid); text != "partial" {
		t.Errorf("entry changed on skipped whitespace payload: %q", text)
	}

	// Already-sealed last entry with MATCHING text → Skipped (MD completed it correctly).
	s2 := NewStreamStore()
	sid2 := "s2"
	s2.AppendDelta(sid2, StreamMeta{MessageID: "m1", ChatID: 1}, 0, "sealed content", true)
	ss2 := s2.Session(sid2)
	ss2.DataMu.Lock()
	ss2.Msgs["m1"].Sealed = true
	ss2.DataMu.Unlock()
	if got := s2.FinalizeLastWithText(sid2, "sealed content"); got != FinalizeSkipped {
		t.Errorf("already-sealed, same text = %v, want FinalizeSkipped", got)
	}
	if text, _, _ := lastText(t, s2, sid2); text != "sealed content" {
		t.Errorf("sealed entry was overwritten: %q", text)
	}

	// Already-sealed last entry with DIFFERENT text → FinalizeSealedMismatch (Stop text was not delivered).
	s3 := NewStreamStore()
	sid3 := "s3"
	s3.AppendDelta(sid3, StreamMeta{MessageID: "m1", ChatID: 1}, 0, "earlier message", true)
	ss3 := s3.Session(sid3)
	ss3.DataMu.Lock()
	ss3.Msgs["m1"].Sealed = true
	ss3.DataMu.Unlock()
	if got := s3.FinalizeLastWithText(sid3, "stop authoritative text"); got != FinalizeSealedMismatch {
		t.Errorf("already-sealed, different text = %v, want FinalizeSealedMismatch", got)
	}
	if text, _, _ := lastText(t, s3, sid3); text != "earlier message" {
		t.Errorf("sealed entry was overwritten on mismatch: %q", text)
	}
}
