package stores

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

func newEntry(uuid, sessionID, toolName, canonical, toolUseID string) *PendingWaitEntry {
	return &PendingWaitEntry{
		UUID:               uuid,
		SessionID:          sessionID,
		ToolName:           toolName,
		ToolInputCanonical: canonical,
		ToolUseID:          toolUseID,
	}
}

// Test 1: FindMatch returns UNRESOLVED-only (a Resolved entry is never matched).
func TestFindMatch_SkipsResolved(t *testing.T) {
	s := NewPendingWaitStore()
	e := newEntry("u1", "sess", "Bash", `{"cmd":"ls"}`, "")
	s.Register(e)
	e.Resolved = true
	_, ok := s.FindMatch("sess", "Bash", `{"cmd":"ls"}`, "")
	if ok {
		t.Fatal("expected no match for resolved entry")
	}
}

// Test 2: FindMatch matches by tool_use_id when present.
func TestFindMatch_ByToolUseID(t *testing.T) {
	s := NewPendingWaitStore()
	e := newEntry("u1", "sess", "Bash", `{"cmd":"ls"}`, "tid-123")
	s.Register(e)
	got, ok := s.FindMatch("sess", "Bash", `{"cmd":"ls"}`, "tid-123")
	if !ok || got.UUID != "u1" {
		t.Fatalf("expected match by tool_use_id, got ok=%v uuid=%v", ok, got)
	}
}

// Test 3: FindMatch matches by (session, tool, canonical) when tool_use_id is "".
func TestFindMatch_ByCanonical(t *testing.T) {
	s := NewPendingWaitStore()
	e := newEntry("u1", "sess", "Bash", `{"cmd":"ls"}`, "")
	s.Register(e)
	got, ok := s.FindMatch("sess", "Bash", `{"cmd":"ls"}`, "")
	if !ok || got.UUID != "u1" {
		t.Fatalf("expected match by canonical, got ok=%v", ok)
	}
}

// Test 4: A non-matching canonical is NOT found.
func TestFindMatch_NoMatch(t *testing.T) {
	s := NewPendingWaitStore()
	e := newEntry("u1", "sess", "Bash", `{"cmd":"ls"}`, "")
	s.Register(e)
	_, ok := s.FindMatch("sess", "Bash", `{"cmd":"pwd"}`, "")
	if ok {
		t.Fatal("expected no match for different canonical")
	}
}

// Test 5: FIFO among identical-input collisions.
func TestFindMatch_FIFO(t *testing.T) {
	s := NewPendingWaitStore()
	e1 := newEntry("u1", "sess", "Bash", `{"cmd":"ls"}`, "")
	e2 := newEntry("u2", "sess", "Bash", `{"cmd":"ls"}`, "")
	s.Register(e1)
	s.Register(e2)
	// First call must return e1 (lowest seq).
	got, ok := s.FindMatch("sess", "Bash", `{"cmd":"ls"}`, "")
	if !ok || got.UUID != "u1" {
		t.Fatalf("expected FIFO first entry u1, got %v", got)
	}
	// Resolve e1 and remove it.
	e1.Resolved = true
	s.Remove("u1")
	// Second call must return e2.
	got, ok = s.FindMatch("sess", "Bash", `{"cmd":"ls"}`, "")
	if !ok || got.UUID != "u2" {
		t.Fatalf("expected FIFO second entry u2, got %v", got)
	}
}

// Test 6: Push with no live reader sets Terminal; TakeTerminal returns it; entry still present.
func TestPush_NoLive_SetsTerminal(t *testing.T) {
	s := NewPendingWaitStore()
	e := newEntry("u1", "sess", "Bash", `{}`, "")
	s.Register(e)
	ev := WaitEvent{Type: "cancel"}
	if !s.Push("u1", ev) {
		t.Fatal("Push returned false")
	}
	// Entry must still exist.
	if _, ok := s.Get("u1"); !ok {
		t.Fatal("entry removed by Push — must not be removed")
	}
	// Terminal must be set.
	got := s.TakeTerminal("u1")
	if got == nil || got.Type != "cancel" {
		t.Fatalf("expected cancel terminal, got %v", got)
	}
	// TakeTerminal clears it.
	if s.TakeTerminal("u1") != nil {
		t.Fatal("expected nil after TakeTerminal cleared it")
	}
}

// Test 7: ClearLive with old gen does NOT clear Live after SetLive with new gen.
func TestClearLive_OldGenNoop(t *testing.T) {
	s := NewPendingWaitStore()
	e := newEntry("u1", "sess", "Bash", `{}`, "")
	s.Register(e)
	s.SetLive("u1", 2, e.Ch)
	s.ClearLive("u1", 1) // old gen — must be a no-op
	got, _ := s.Get("u1")
	if !got.Live {
		t.Fatal("Live was cleared by old gen — should not happen")
	}
	s.ClearLive("u1", 2) // correct gen — must clear
	if got.Live {
		t.Fatal("Live was not cleared by correct gen")
	}
}

// Test 8: Grace cancel keyed to gen N is a no-op after BumpGeneration advances to N+1.
func TestBumpGeneration_StaleGenNoop(t *testing.T) {
	s := NewPendingWaitStore()
	e := newEntry("u1", "sess", "Bash", `{}`, "")
	s.Register(e)
	gen := s.BumpGeneration("u1") // gen == 1
	s.SetLive("u1", gen, e.Ch)
	newGen := s.BumpGeneration("u1") // gen == 2
	// A handler from gen 1 tries to cancel — CurrentGeneration != 1.
	if s.CurrentGeneration("u1") == gen {
		t.Fatalf("expected generation to have advanced past %d", gen)
	}
	if s.CurrentGeneration("u1") != newGen {
		t.Fatalf("expected current generation %d, got %d", newGen, s.CurrentGeneration("u1"))
	}
}

// Test 9: SweepUndelivered returns only entries whose Terminal is older than TTL.
func TestSweepUndelivered(t *testing.T) {
	s := NewPendingWaitStore()
	e1 := newEntry("u1", "sess", "Bash", `{}`, "")
	e2 := newEntry("u2", "sess", "Bash", `{}`, "")
	s.Register(e1)
	s.Register(e2)
	ev := WaitEvent{Type: "cancel"}
	s.Push("u1", ev)
	s.Push("u2", ev)
	// Force e1's ResolvedAt to be old enough.
	s.mu.Lock()
	s.entries["u1"].ResolvedAt = 0 // far in the past
	s.mu.Unlock()
	// e2's ResolvedAt is just now — should not appear with ttl=1.
	swept := s.SweepUndelivered(1)
	if len(swept) != 1 || swept[0] != "u1" {
		t.Fatalf("expected only u1 in sweep, got %v", swept)
	}
}

// Test 10: Push never blocks (call with full channel and no reader).
func TestPush_NeverBlocks(t *testing.T) {
	s := NewPendingWaitStore()
	e := newEntry("u1", "sess", "Bash", `{}`, "")
	s.Register(e)
	// Mark as Live so Push tries to send on Ch (which is empty, capacity 1).
	s.SetLive("u1", 1, e.Ch)
	done := make(chan struct{})
	go func() {
		// Fill the channel so second push would block if not non-blocking.
		raw, _ := json.Marshal("data")
		s.Push("u1", WaitEvent{Type: "answer", Output: json.RawMessage(raw)})
		// Reset Resolved to allow a second push attempt on the same channel.
		s.mu.Lock()
		s.entries["u1"].Resolved = false
		s.mu.Unlock()
		// Channel is now full (cap 1); Push must not block.
		s.Push("u1", WaitEvent{Type: "cancel"})
		close(done)
	}()
	select {
	case <-done:
		// ok
	default:
		// done closed synchronously — also ok
		<-done
	}
}

// Test 11: ResolveIfUnresolved CAS — first resolver wins, second loses.
func TestResolveIfUnresolved_CAS(t *testing.T) {
	s := NewPendingWaitStore()
	e := newEntry("u1", "sess", "AskUserQuestion", `{}`, "")
	s.Register(e)

	// First resolver wins
	won, snap, found := s.ResolveIfUnresolved("u1", WaitEvent{Type: "answer"})
	if !won || !found || !snap.Resolved {
		t.Fatalf("expected first resolver to win: won=%v found=%v resolved=%v", won, found, snap.Resolved)
	}

	// Second resolver loses
	won2, snap2, found2 := s.ResolveIfUnresolved("u1", WaitEvent{Type: "cancel"})
	if won2 || !found2 || !snap2.Resolved {
		t.Fatalf("expected second resolver to lose: won=%v found=%v resolved=%v", won2, found2, snap2.Resolved)
	}
}

func TestResolveIfUnresolved_NotFound(t *testing.T) {
	s := NewPendingWaitStore()
	won, _, found := s.ResolveIfUnresolved("nonexistent", WaitEvent{Type: "cancel"})
	if won || found {
		t.Fatal("expected not found")
	}
}

// Test 12: FindByTmuxTarget FIFO.
func TestFindByTmuxTarget_FIFO(t *testing.T) {
	s := NewPendingWaitStore()
	e1 := &PendingWaitEntry{UUID: "u1", TmuxTarget: "%5", SessionID: "s1"}
	e2 := &PendingWaitEntry{UUID: "u2", TmuxTarget: "%5", SessionID: "s1"}
	s.Register(e1)
	s.Register(e2)

	snap, ok := s.FindByTmuxTarget("%5")
	if !ok || snap.UUID != "u1" {
		t.Fatalf("expected FIFO u1 first, got %v", snap)
	}

	// Resolve u1
	s.ResolveIfUnresolved("u1", WaitEvent{Type: "cancel"})
	snap2, ok2 := s.FindByTmuxTarget("%5")
	if !ok2 || snap2.UUID != "u2" {
		t.Fatalf("expected u2 after u1 resolved, got %v", snap2)
	}
}

func TestFindByTmuxTarget_SkipsResolved(t *testing.T) {
	s := NewPendingWaitStore()
	e := &PendingWaitEntry{UUID: "u1", TmuxTarget: "%5"}
	s.Register(e)
	s.ResolveIfUnresolved("u1", WaitEvent{Type: "cancel"})

	_, ok := s.FindByTmuxTarget("%5")
	if ok {
		t.Fatal("expected no match for resolved entry")
	}
}

// Test 13: BeginLive sets Live flag and returns channel + generation.
func TestBeginLive(t *testing.T) {
	s := NewPendingWaitStore()
	e := newEntry("u1", "sess", "Bash", `{}`, "")
	s.Register(e)

	snap, ch, gen, found := s.BeginLive("u1")
	if !found || ch == nil || gen == 0 {
		t.Fatalf("expected BeginLive success: found=%v gen=%d", found, gen)
	}
	if snap.UUID != "u1" {
		t.Fatalf("expected snapshot UUID u1, got %s", snap.UUID)
	}

	// Entry should be Live
	entry, _ := s.Get("u1")
	if !entry.Live || entry.LiveGen != gen {
		t.Fatalf("expected Live=true LiveGen=%d, got Live=%v LiveGen=%d", gen, entry.Live, entry.LiveGen)
	}
}

// Test 14: PeekTerminal/ClearTerminal/RestoreTerminal round-trip.
func TestPeekClearRestoreTerminal(t *testing.T) {
	s := NewPendingWaitStore()
	e := newEntry("u1", "sess", "Bash", `{}`, "")
	s.Register(e)

	// No terminal yet
	if s.PeekTerminal("u1") != nil {
		t.Fatal("expected nil terminal")
	}

	// Push sets terminal (not live)
	s.Push("u1", WaitEvent{Type: "cancel"})

	// Peek returns copy without clearing
	term := s.PeekTerminal("u1")
	if term == nil || term.Type != "cancel" {
		t.Fatalf("expected cancel terminal, got %v", term)
	}
	term2 := s.PeekTerminal("u1")
	if term2 == nil {
		t.Fatal("PeekTerminal cleared the terminal — should not")
	}

	// ClearTerminal removes it
	s.ClearTerminal("u1")
	if s.PeekTerminal("u1") != nil {
		t.Fatal("expected nil after ClearTerminal")
	}

	// RestoreTerminal puts it back
	ev := WaitEvent{Type: "answer"}
	s.RestoreTerminal("u1", &ev)
	restored := s.PeekTerminal("u1")
	if restored == nil || restored.Type != "answer" {
		t.Fatalf("expected answer terminal, got %v", restored)
	}
}

// Test 15: Snapshot methods.
func TestGetSnapshot(t *testing.T) {
	s := NewPendingWaitStore()
	e := &PendingWaitEntry{UUID: "u1", SessionID: "sess", ToolName: "Bash", MsgID: 42, ChatID: 100}
	s.Register(e)

	snap, ok := s.GetSnapshot("u1")
	if !ok || snap.UUID != "u1" || snap.MsgID != 42 || snap.ChatID != 100 {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
}

func TestFindByMsgIDSnapshot(t *testing.T) {
	s := NewPendingWaitStore()
	e := &PendingWaitEntry{UUID: "u1", MsgID: 42}
	s.Register(e)

	snap, ok := s.FindByMsgIDSnapshot(42)
	if !ok || snap.UUID != "u1" {
		t.Fatalf("expected match by MsgID")
	}

	_, ok2 := s.FindByMsgIDSnapshot(999)
	if ok2 {
		t.Fatal("expected no match")
	}
}

func TestFindBySessionSnapshots(t *testing.T) {
	s := NewPendingWaitStore()
	s.Register(&PendingWaitEntry{UUID: "u1", SessionID: "sess1"})
	s.Register(&PendingWaitEntry{UUID: "u2", SessionID: "sess1"})
	s.Register(&PendingWaitEntry{UUID: "u3", SessionID: "sess2"})

	snaps := s.FindBySessionSnapshots("sess1")
	if len(snaps) != 2 {
		t.Fatalf("expected 2 snapshots for sess1, got %d", len(snaps))
	}
}

// Test 16: Lifecycle — entry removed after delivery.
func TestLifecycle_EntryRemovedAfterDelivery(t *testing.T) {
	s := NewPendingWaitStore()
	e := newEntry("u1", "sess", "AskUserQuestion", `{}`, "")
	s.Register(e)

	// Resolve
	won, _, _ := s.ResolveIfUnresolved("u1", WaitEvent{Type: "answer"})
	if !won {
		t.Fatal("expected to win CAS")
	}

	// Terminal should be set (not live)
	term := s.PeekTerminal("u1")
	if term == nil || term.Type != "answer" {
		t.Fatal("expected answer terminal")
	}

	// Simulate delivery + cleanup
	s.ClearTerminal("u1")
	s.Remove("u1")

	_, ok := s.Get("u1")
	if ok {
		t.Fatal("entry should be removed after delivery")
	}
}

// Test 17: ResolveIfUnresolved while live delivers to channel.
func TestResolveIfUnresolved_LiveDelivery(t *testing.T) {
	s := NewPendingWaitStore()
	e := newEntry("u1", "sess", "AskUserQuestion", `{}`, "")
	s.Register(e)

	// BeginLive
	_, ch, _, found := s.BeginLive("u1")
	if !found || ch == nil {
		t.Fatal("expected BeginLive success")
	}

	// Resolve while live — should deliver to channel
	won, _, _ := s.ResolveIfUnresolved("u1", WaitEvent{Type: "answer"})
	if !won {
		t.Fatal("expected to win CAS")
	}

	// Check channel received it
	select {
	case ev := <-ch:
		if ev.Type != "answer" {
			t.Fatalf("expected answer event, got %s", ev.Type)
		}
	default:
		t.Fatal("expected event on channel")
	}
}

// Test 18: BackfillMsgID sets MsgID, ChatID, and TopicID on an existing entry.
func TestBackfillMsgID(t *testing.T) {
	s := NewPendingWaitStore()
	e := &PendingWaitEntry{UUID: "u1", SessionID: "sess", MsgID: 0}
	s.Register(e)

	s.BackfillMsgID("u1", 42, 100, 5, "backfilled text")

	snap, ok := s.FindByMsgIDSnapshot(42)
	if !ok {
		t.Fatal("expected entry found by MsgID=42")
	}
	if snap.MsgID != 42 {
		t.Fatalf("expected MsgID=42, got %d", snap.MsgID)
	}
	if snap.ChatID != 100 {
		t.Fatalf("expected ChatID=100, got %d", snap.ChatID)
	}
	if snap.TopicID != 5 {
		t.Fatalf("expected TopicID=5, got %d", snap.TopicID)
	}
}

// Test 19: Pre-send window — AskUserQuestion entry has Questions populated and MsgID==0.
func TestPreSendWindowAskQ(t *testing.T) {
	s := NewPendingWaitStore()
	e := &PendingWaitEntry{
		UUID:       "u1",
		TmuxTarget: "%10",
		ToolName:   "AskUserQuestion",
		MsgID:      0,
		Questions: []QuestionMeta{
			{
				QuestionText: "Confirm?",
				Header:       "Confirmation",
				NumOptions:   2,
				OptionLabels: []string{"Yes", "No"},
				MultiSelect:  false,
			},
		},
	}
	s.Register(e)

	snap, ok := s.FindByTmuxTarget("%10")
	if !ok {
		t.Fatal("expected entry found by TmuxTarget")
	}
	if snap.ToolName != "AskUserQuestion" {
		t.Fatalf("expected ToolName=AskUserQuestion, got %s", snap.ToolName)
	}
	if len(snap.Questions) == 0 {
		t.Fatal("expected Questions to be non-empty")
	}
	if snap.MsgID != 0 {
		t.Fatalf("expected MsgID=0, got %d", snap.MsgID)
	}
}

// Test 20: Pre-send window — PermissionRequest entry has PermSuggestions populated and MsgID==0.
func TestPreSendWindowPermReq(t *testing.T) {
	s := NewPendingWaitStore()
	e := &PendingWaitEntry{
		UUID:            "u1",
		TmuxTarget:      "%11",
		ToolName:        "PermissionRequest",
		MsgID:           0,
		PermSuggestions: json.RawMessage(`[{"label":"Allow"}]`),
	}
	s.Register(e)

	snap, ok := s.FindByTmuxTarget("%11")
	if !ok {
		t.Fatal("expected entry found by TmuxTarget")
	}
	if snap.ToolName != "PermissionRequest" {
		t.Fatalf("expected ToolName=PermissionRequest, got %s", snap.ToolName)
	}
	if len(snap.PermSuggestions) == 0 {
		t.Fatal("expected PermSuggestions to be non-empty")
	}
}

// Test 21: ToggleQuestionOption toggles a multi-select option on and off.
func TestToggleQuestionOption(t *testing.T) {
	s := NewPendingWaitStore()
	e := &PendingWaitEntry{
		UUID: "u1",
		Questions: []QuestionMeta{
			{
				QuestionText:    "Pick one or more",
				MultiSelect:     true,
				OptionLabels:    []string{"A", "B", "C"},
				SelectedOptions: make(map[int]bool),
			},
		},
	}
	s.Register(e)

	// Toggle option 0 on
	qs, err := s.ToggleQuestionOption("u1", 0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !qs[0].SelectedOptions[0] {
		t.Fatal("expected option 0 selected after first toggle")
	}

	// Toggle option 0 off
	qs, err = s.ToggleQuestionOption("u1", 0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if qs[0].SelectedOptions[0] {
		t.Fatal("expected option 0 deselected after second toggle")
	}
}

// Test 22: SelectQuestionOption sets the SelectedOption for a single-choice question.
func TestSelectQuestionOption(t *testing.T) {
	s := NewPendingWaitStore()
	e := &PendingWaitEntry{
		UUID: "u1",
		Questions: []QuestionMeta{
			{
				QuestionText: "Pick one",
				MultiSelect:  false,
				OptionLabels: []string{"X", "Y", "Z"},
			},
		},
	}
	s.Register(e)

	qs, err := s.SelectQuestionOption("u1", 0, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if qs[0].SelectedOption != 2 {
		t.Fatalf("expected SelectedOption=2, got %d", qs[0].SelectedOption)
	}
}

// Test 23: FindByTmuxTarget normalizes via notify.FormatPaneID (identity) — same string finds entry.
// FormatPaneID is an identity function, so storing and querying with the same string works correctly.
func TestFindByTmuxTargetNormalization(t *testing.T) {
	s := NewPendingWaitStore()
	target := "%42"
	e := &PendingWaitEntry{UUID: "u1", TmuxTarget: target}
	s.Register(e)

	// Both stored and queried values pass through FormatPaneID (identity), so they match.
	snap, ok := s.FindByTmuxTarget(target)
	if !ok {
		t.Fatal("expected entry found by TmuxTarget")
	}
	if snap.UUID != "u1" {
		t.Fatalf("expected UUID=u1, got %s", snap.UUID)
	}
}

// Test 24: EntrySnapshot includes all 3 merged fields: Questions, MsgText, PermSuggestions.
func TestEntrySnapshotMergedFields(t *testing.T) {
	s := NewPendingWaitStore()
	e := &PendingWaitEntry{
		UUID:    "u1",
		MsgText: "hello world",
		Questions: []QuestionMeta{
			{QuestionText: "Q1", OptionLabels: []string{"Opt1"}},
		},
		PermSuggestions: json.RawMessage(`[{"label":"Allow"}]`),
	}
	s.Register(e)

	snap, ok := s.GetSnapshot("u1")
	if !ok {
		t.Fatal("expected snapshot found")
	}
	if snap.MsgText != "hello world" {
		t.Fatalf("expected MsgText='hello world', got '%s'", snap.MsgText)
	}
	if len(snap.Questions) == 0 {
		t.Fatal("expected Questions non-empty in snapshot")
	}
	if len(snap.PermSuggestions) == 0 {
		t.Fatal("expected PermSuggestions non-empty in snapshot")
	}
}

// Test 25: Concurrent ToggleQuestionOption and GetSnapshot must not data-race.
func TestQuestionDeepCopyRace(t *testing.T) {
	s := NewPendingWaitStore()
	e := &PendingWaitEntry{
		UUID: "u1",
		Questions: []QuestionMeta{
			{
				QuestionText:    "Race?",
				MultiSelect:     true,
				OptionLabels:    []string{"A", "B"},
				SelectedOptions: make(map[int]bool),
			},
		},
	}
	s.Register(e)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Goroutine A: repeatedly toggles option
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				s.ToggleQuestionOption("u1", 0, 0) //nolint:errcheck
			}
		}
	}()

	// Goroutine B: repeatedly snapshots
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				s.GetSnapshot("u1")
			}
		}
	}()

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// (Tests 26-30 removed — they tested NotifOpQueue which has been retired)
