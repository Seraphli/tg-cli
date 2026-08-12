package stores

import "testing"

// interruptStateOf reports (Interrupted, InterruptRendered) for an entry, under DataMu (white-box helper).
func interruptStateOf(s *StreamStore, sid, mid string) (interrupted, rendered bool) {
	ss := s.Session(sid)
	ss.DataMu.Lock()
	defer ss.DataMu.Unlock()
	if e := ss.Msgs[mid]; e != nil {
		return e.Interrupted, e.InterruptRendered
	}
	return false, false
}

func setInterruptRendered(s *StreamStore, sid, mid string) {
	ss := s.Session(sid)
	ss.DataMu.Lock()
	defer ss.DataMu.Unlock()
	if e := ss.Msgs[mid]; e != nil {
		e.InterruptRendered = true
	}
}

// TestMarkInterruptedRetry covers Round-4 Item 7: the agent_retry hook marks the LAST rendered bubble of the
// retrying turn interrupted-and-retrying. On the ordered Hook FIFO the retry's own bubble does not exist yet,
// so the last entry of turn_id is the errored (truncated) one. The mark must be turn-scoped (never touch a
// different turn's bubble), pick the LAST entry of that turn, no-op on an unknown session/turn, and be one-shot
// (skipped once the bubble has been rendered-marked).
func TestMarkInterruptedRetry(t *testing.T) {
	// unknown session -> false
	if NewStreamStore().MarkInterruptedRetry("nope", "t1") {
		t.Fatal("MarkInterruptedRetry on an unknown session must be false")
	}

	// single bubble on t1 -> marked (Interrupted set); a turn with no bubble -> false.
	s := NewStreamStore()
	const sid = "s1"
	s.AppendDelta(sid, StreamMeta{MessageID: "m1", TurnID: "t1", ChatID: 1}, 0, "half a sentence", false)
	if !s.MarkInterruptedRetry(sid, "t1") {
		t.Fatal("expected the t1 bubble to be marked")
	}
	if in, _ := interruptStateOf(s, sid, "m1"); !in {
		t.Error("m1.Interrupted should be true after MarkInterruptedRetry")
	}
	if s.MarkInterruptedRetry(sid, "t2") {
		t.Error("MarkInterruptedRetry for a turn with no bubble must be false")
	}

	// LAST entry of the turn is chosen, and a later different-turn bubble is left untouched.
	s = NewStreamStore()
	s.AppendDelta(sid, StreamMeta{MessageID: "a1", TurnID: "t1", ChatID: 1}, 0, "first t1 bubble", false)
	s.AppendDelta(sid, StreamMeta{MessageID: "a2", TurnID: "t1", ChatID: 1}, 0, "second t1 bubble (the truncated one)", false)
	s.AppendDelta(sid, StreamMeta{MessageID: "b1", TurnID: "t2", ChatID: 1}, 0, "a later-turn bubble", false)
	if !s.MarkInterruptedRetry(sid, "t1") {
		t.Fatal("expected a t1 bubble to be marked")
	}
	if in, _ := interruptStateOf(s, sid, "a2"); !in {
		t.Error("the LAST t1 entry (a2) must be the one marked")
	}
	if in, _ := interruptStateOf(s, sid, "a1"); in {
		t.Error("an earlier t1 entry (a1) must NOT be marked")
	}
	if in, _ := interruptStateOf(s, sid, "b1"); in {
		t.Error("a different-turn entry (b1) must NOT be marked")
	}

	// one-shot: once the bubble has been rendered-marked, a re-POST does not re-mark.
	setInterruptRendered(s, sid, "a2")
	if s.MarkInterruptedRetry(sid, "t1") {
		t.Error("MarkInterruptedRetry must be a no-op once the bubble was already rendered-marked")
	}
}
