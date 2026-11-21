package stores

import (
	"strings"
	"testing"
)

// --- white-box helpers (armedInOrder + lastText live in stream_finalize_test.go) ---

func orderLen(s *StreamStore, sid string) int {
	ss := s.Session(sid)
	ss.DataMu.Lock()
	defer ss.DataMu.Unlock()
	return len(ss.Order)
}

func orderCountOf(s *StreamStore, sid, mid string) int {
	ss := s.Session(sid)
	ss.DataMu.Lock()
	defer ss.DataMu.Unlock()
	n := 0
	for _, id := range ss.Order {
		if id == mid {
			n++
		}
	}
	return n
}

func classOf(s *StreamStore, sid, mid string) StopClass {
	ss := s.Session(sid)
	ss.DataMu.Lock()
	defer ss.DataMu.Unlock()
	if e := ss.Msgs[mid]; e != nil {
		return e.StopClass
	}
	return StopClassUnclassified
}

func assembledOf(s *StreamStore, sid, mid string) string {
	ss := s.Session(sid)
	ss.DataMu.Lock()
	defer ss.DataMu.Unlock()
	if e := ss.Msgs[mid]; e != nil {
		txt, _ := e.AssembledText()
		return txt
	}
	return ""
}

func hasEntry(s *StreamStore, sid, mid string) bool {
	ss := s.Session(sid)
	ss.DataMu.Lock()
	defer ss.DataMu.Unlock()
	_, ok := ss.Msgs[mid]
	return ok
}

func chatIDOf(s *StreamStore, sid, mid string) int64 {
	ss := s.Session(sid)
	ss.DataMu.Lock()
	defer ss.DataMu.Unlock()
	if e := ss.Msgs[mid]; e != nil {
		return e.ChatID
	}
	return 0
}

// stopSession primes a session with an authoritative Stop body and marks it Stopped.
func stopSession(s *StreamStore, sid, body string) {
	s.AppendDelta(sid, StreamMeta{MessageID: "m0", TurnID: "T0", ChatID: 1}, 0, body, true)
	s.FinalizeLastWithText(sid, body)
	s.MarkStopped(sid)
}

// --- P1 classification: a copy is held until text outside the Stop body arrives; NEW is sticky ---

// (i) every non-empty delta contained in the Stop body → provisional STOP-COPY: held (stored, never armed, no
// ss.Order distortion); a later delta that is NOT in the Stop body turns the message NEW for good and arms it.
func TestSticky_StopCopy_HeldUntilNewText(t *testing.T) {
	s := NewStreamStore()
	sid := "s"
	stopSession(s, sid, "AUTHORITATIVE BODY")
	orderBefore := orderLen(s, sid)

	if h, d, post := s.AppendExisting(sid, "mc", "Tc", 0, "AUTHORITATIVE", false); !h || !d || post != PostStopDrop {
		t.Fatalf("classify stop-copy: want drop+PostStopDrop, got %v %v %v", h, d, post)
	}
	if classOf(s, sid, "mc") != StopClassStopCopy || armedInOrder(s, sid, "mc") || orderLen(s, sid) != orderBefore {
		t.Fatal("a held copy must stay out of ss.Order")
	}
	if h, d, post := s.AppendExisting(sid, "mc", "Tc", 1, " BODY", true); !h || !d || post != PostStopDrop {
		t.Fatalf("a delta inside the Stop body keeps the message held, got %v %v %v", h, d, post)
	}
	if h, d, post := s.AppendExisting(sid, "mn2", "Tc", 0, "AUTHORITATIVE", false); !h || !d || post != PostStopDrop {
		t.Fatalf("second message: want held, got %v %v %v", h, d, post)
	}
	if _, d, post := s.AppendExisting(sid, "mn2", "Tc", 1, " and more", true); d || post != PostStopNeedsArm {
		t.Fatalf("text not in the Stop body: want NeedsArm, got dropped=%v post=%v", d, post)
	}
	if classOf(s, sid, "mn2") != StopClassNew || assembledOf(s, sid, "mn2") != "AUTHORITATIVE and more" {
		t.Fatalf("NEW message must keep the held delta, got %q", assembledOf(s, sid, "mn2"))
	}
	// every piece inside the Stop body, but the message as a whole is not (the pieces swapped) → NEW.
	if h, d, post := s.AppendExisting(sid, "mn3", "Tc", 0, "BODY", false); !h || !d || post != PostStopDrop {
		t.Fatalf("third message: want held, got %v %v %v", h, d, post)
	}
	if _, d, post := s.AppendExisting(sid, "mn3", "Tc", 1, "AUTHORITATIVE", true); d || post != PostStopNeedsArm {
		t.Fatalf("pieces inside the Stop body but joined text outside it: want NeedsArm, got dropped=%v post=%v", d, post)
	}
}

// (ii) first non-empty delta NOT contained → NEW: later fragments that happen to look like the Stop body are
// still delivered whole (no re-matching).
func TestSticky_New_KeepsAll(t *testing.T) {
	s := NewStreamStore()
	sid := "s"
	stopSession(s, sid, "STOP BODY")

	if _, _, post := s.AppendExisting(sid, "mn", "Tn", 0, "genuinely new intro", false); post != PostStopNeedsArm {
		t.Fatalf("classify new: want NeedsArm, got %v", post)
	}
	s.ArmStopped(sid, "mn", StreamMeta{MessageID: "mn", ChatID: 1})
	// a later fragment equal to the Stop body must STILL be delivered (sticky NEW, no re-match).
	if _, _, post := s.AppendExisting(sid, "mn", "Tn", 1, "STOP BODY", true); post != PostStopArmedComplete {
		t.Fatalf("new sticky: stop-body-looking fragment must still be delivered, got %v", post)
	}
	if txt := assembledOf(s, sid, "mn"); !strings.Contains(txt, "genuinely new intro") || !strings.Contains(txt, "STOP BODY") {
		t.Fatalf("NEW message must retain all fragments whole, got %q", txt)
	}
}

// (iii) leading empty/whitespace deltas defer (never classify); the first real delta then classifies NEW.
// Separately, a final flag on an empty delta before any content is a deferred no-op (final-before-content).
func TestSticky_EmptyFirstDefers(t *testing.T) {
	s := NewStreamStore()
	sid := "s"
	stopSession(s, sid, "BODY")

	// leading empty (non-final) delta defers without classifying.
	if _, d, post := s.AppendExisting(sid, "me", "Te", 0, "   ", false); d || post != PostStopDefer {
		t.Fatalf("empty-first: want defer no-drop, got dropped=%v post=%v", d, post)
	}
	if classOf(s, sid, "me") != StopClassUnclassified {
		t.Fatal("empty delta must not classify")
	}
	// the first real (non-empty) delta classifies NEW and requests arm.
	if _, _, post := s.AppendExisting(sid, "me", "Te", 1, "real content", false); post != PostStopNeedsArm {
		t.Fatalf("first non-empty after empties: want NeedsArm, got %v", post)
	}
	if classOf(s, sid, "me") != StopClassNew {
		t.Fatal("first non-empty delta must classify NEW")
	}

	// final-before-content: a final flag on an empty delta before any content is a deferred no-op — it never
	// classifies and never arms (the reachable assembled text stays empty).
	if _, d, post := s.AppendExisting(sid, "mf", "Tf", 0, "", true); d || post != PostStopDefer {
		t.Fatalf("final-before-content: want defer no-drop, got dropped=%v post=%v", d, post)
	}
	if classOf(s, sid, "mf") != StopClassUnclassified || armedInOrder(s, sid, "mf") {
		t.Fatal("empty final-before-content must neither classify nor arm")
	}
}

// (iv) an all-empty message is a permanent no-op: never classifies, never arms.
func TestSticky_AllEmpty_NoOp(t *testing.T) {
	s := NewStreamStore()
	sid := "s"
	stopSession(s, sid, "BODY")
	for i, final := range []bool{false, false, true} {
		if _, d, post := s.AppendExisting(sid, "mz", "Tz", i, "", final); d || post != PostStopDefer {
			t.Fatalf("all-empty delta %d: want defer no-drop, got dropped=%v post=%v", i, d, post)
		}
	}
	if armedInOrder(s, sid, "mz") {
		t.Fatal("all-empty message must never arm (permanent no-op)")
	}
	if classOf(s, sid, "mz") != StopClassUnclassified {
		t.Fatal("all-empty message must never classify")
	}
}

// --- P2 seams ---

// idx1-first: the NEW entry stays out of ss.Order until index 0 fills, then arms exactly once retaining the
// idx1 content + FinalIdx.
func TestSeam_Idx1First_ArmsOnceAfterIdx0(t *testing.T) {
	s := NewStreamStore()
	sid := "s"
	stopSession(s, sid, "BODY")

	// index 1 (non-empty, final) arrives first → classified NEW but assembled has a gap at 0 → deferred.
	if _, _, post := s.AppendExisting(sid, "mB", "TB", 1, "second", true); post != PostStopDefer {
		t.Fatalf("idx1-first: want defer (gap at 0), got %v", post)
	}
	if classOf(s, sid, "mB") != StopClassNew {
		t.Fatal("idx1-first: first non-empty delta must classify NEW")
	}
	if armedInOrder(s, sid, "mB") {
		t.Fatal("idx1-first: must not arm while index 0 is missing")
	}
	// index 0 fills → assembled non-empty → NeedsArm.
	if _, _, post := s.AppendExisting(sid, "mB", "TB", 0, "first ", false); post != PostStopNeedsArm {
		t.Fatalf("idx0 fill: want NeedsArm, got %v", post)
	}
	armed, complete := s.ArmStopped(sid, "mB", StreamMeta{MessageID: "mB", ChatID: 1})
	if !armed || !complete {
		t.Fatalf("arm after idx0: want armed=true complete=true (FinalIdx=1 reached), got %v %v", armed, complete)
	}
	if n := orderCountOf(s, sid, "mB"); n != 1 {
		t.Fatalf("mB must appear exactly once in ss.Order, got %d", n)
	}
	if txt := assembledOf(s, sid, "mB"); txt != "first second" {
		t.Fatalf("idx1-first must retain the idx1 content: assembled=%q", txt)
	}
}

// ResolveChat failure path: DropStopped removes the entry from both ss.Msgs and ss.Order.
func TestSeam_DropStopped_RemovesMapAndOrder(t *testing.T) {
	s := NewStreamStore()
	sid := "s"
	stopSession(s, sid, "BODY")
	s.AppendExisting(sid, "mn", "Tn", 0, "new", false)
	s.ArmStopped(sid, "mn", StreamMeta{MessageID: "mn", ChatID: 1})
	if !armedInOrder(s, sid, "mn") {
		t.Fatal("precondition: mn should be armed")
	}
	s.DropStopped(sid, "mn")
	if armedInOrder(s, sid, "mn") {
		t.Fatal("DropStopped must sweep mn from ss.Order")
	}
	if hasEntry(s, sid, "mn") {
		t.Fatal("DropStopped must delete mn from ss.Msgs")
	}
}

// metadata-before-visible: an unarmed entry has zero ChatID; once armed it is simultaneously in ss.Order AND
// carrying full metadata (no in-Order-without-metadata window, since ArmStopped does both under one DataMu hold).
func TestSeam_ArmStopped_MetadataInstalled(t *testing.T) {
	s := NewStreamStore()
	sid := "s"
	stopSession(s, sid, "BODY")
	s.AppendExisting(sid, "mn", "Tn", 0, "content", false)
	if got := chatIDOf(s, sid, "mn"); got != 0 {
		t.Fatalf("pre-arm ChatID should be 0, got %d", got)
	}
	if armedInOrder(s, sid, "mn") {
		t.Fatal("pre-arm entry must be absent from ss.Order")
	}
	s.ArmStopped(sid, "mn", StreamMeta{MessageID: "mn", ChatID: 4242, TmuxTarget: "%9", Backend: "cc", Project: "P"})
	if !armedInOrder(s, sid, "mn") {
		t.Fatal("armed entry must be in ss.Order")
	}
	if got := chatIDOf(s, sid, "mn"); got != 4242 {
		t.Fatalf("armed entry must carry ChatID metadata, got %d", got)
	}
}

// P6 tombstone survives Rotate: a NEW turn observed in the stopped handler is tombstoned, so a straggler of
// that turn arriving after a Rotate (which keeps ClosedTurns) is dropped by turnClosed.
func TestSeam_PostStopTombstone_SurvivesRotate(t *testing.T) {
	s := NewStreamStore()
	sid := "s"
	stopSession(s, sid, "BODY") // tombstones T0
	// a late MD of a NEW turn T1 in the stopped handler records T1's tombstone (P6).
	s.AppendExisting(sid, "mn", "T1", 0, "new content", false)
	s.Rotate(sid, "next-prompt") // new user turn — keeps ClosedTurns
	// a straggler of the tombstoned turn T1 after Rotate (session no longer Stopped) must be dropped.
	if h, d, post := s.AppendExisting(sid, "mn2", "T1", 0, "late straggler", true); !h || !d || post != PostStopNone {
		t.Fatalf("post-Rotate straggler of tombstoned turn T1: want handled+dropped+None, got %v %v %v", h, d, post)
	}
}
