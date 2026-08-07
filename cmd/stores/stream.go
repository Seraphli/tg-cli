package stores

import (
	"encoding/json"
	"strings"
	"sync"
	"time"
)

type StreamMeta struct {
	MessageID, TurnID, PromptID, TmuxTarget, Project, CWD, AgentName, Backend string
	ChatID                                                                    int64
	TopicID                                                                   int
}

// StreamEntry is one streamed assistant message (one message_id).
// Deltas/FinalIdx/Dirty/LastFlush/PositionedChunks are guarded by SessionStream.DataMu.
// Msgs/SentText/TablesSent are mutated only by flush (serialized by SessionStream.FlushMu).
// Sealed is normally set by flush (FlushMu), but FinalizeLastWithText also sets it under DataMu to
// install the authoritative Stop text and slam the drop-barrier on late MD — both writers are safe
// because AppendDelta/AppendExisting read Sealed under the same DataMu.
type StreamEntry struct {
	MessageID  string
	TurnID     string
	PromptID   string
	ChatID     int64
	TopicID    int
	TmuxTarget string
	Project    string
	CWD        string
	AgentName  string
	Backend    string
	Deltas     map[int]string // index → delta (reassembles out-of-order async arrival)
	FinalIdx   int            // index carrying final:true; -1 until seen
	Msgs       []int          // TG message ids (continuation chain)
	SentText   []string       // last text rendered into each TG msg (skip unchanged edits)
	TablesSent bool
	Sealed     bool // content done (no more delta / tables sent) — NOT the same as Stop relabel
	Relabeled  bool // Stop header 💬→✅ already applied to this (last) message
	Rich       bool // true = message was first sent as rich (Bot API 10.1); all edits must match
	Dirty      bool
	LastFlush  time.Time
	// PositionedChunks is how many chunks have been placed/queued for this bubble so far (the enqueue-time
	// high-water-mark, f25). flushSession reads it at snapshot (p1) and commits planned=len(chunks) at a
	// SUCCESSFUL enqueue (p3) under DataMu — NOT at render completion — so a second snapshot under Message-FIFO
	// lag sees the already-queued count and does not re-split. A planned chunk count exceeding this means a NEW
	// TG message will be SENT below (new bubble or pagination growth) -> SendBelowSinceTool.
	PositionedChunks int
	// StopClass is the sticky post-Stop classification (commit 18) of an entry CREATED by the post-Stop state
	// machine while ss.Stopped. Decided exactly once at the first non-empty delta: StopCopy (a verbatim
	// paragraph run of the authoritative Stop body — every delta dropped, never armed) or New (a genuinely new
	// message — streamed progressively once armed). Unclassified until then. Irrelevant for entries created by
	// the normal AppendDelta path (they are armed from birth and never consult it).
	StopClass StopClass
}

// StopClass is the sticky classification of a post-Stop MessageDisplay entry (commit 18).
type StopClass int

const (
	StopClassUnclassified StopClass = iota // no non-empty delta seen yet (or a normal pre-Stop entry)
	StopClassStopCopy                      // verbatim paragraph run of the Stop body → drop every delta, never arm
	StopClassNew                           // genuinely new post-Stop message → stream progressively once armed
)

// AssembledText returns contiguous text from index 0 and whether the message is complete.
func (e *StreamEntry) AssembledText() (string, bool) {
	var b strings.Builder
	for i := 0; ; i++ {
		d, ok := e.Deltas[i]
		if !ok {
			return b.String(), false
		}
		b.WriteString(d)
		if e.FinalIdx >= 0 && i == e.FinalIdx {
			return b.String(), true
		}
	}
}

// PostToolSignal records a PostToolUse (or PostToolUseFailure) arrival, set off-FIFO in the /hook/ HTTP
// handler before the SessionEvents.Dispatch, so the PreToolUse bounded wait can detect it (B2) and — when
// PostToolUse beat PreToolUse under async — apply the buffered result edit inline.
type PostToolSignal struct {
	ToolName string
	Response json.RawMessage
	At       time.Time
}

// ToolNotifyMark records the last PreToolUse tool-notification send, so a same-prompt MessageDisplay-final
// arriving shortly after can be logged as a residual inversion (observability only).
type ToolNotifyMark struct {
	PromptID string
	At       time.Time
}

type SessionStream struct {
	DataMu        sync.Mutex // guards Msgs/Order/Stopped/Closed + per-entry Deltas/FinalIdx/Dirty/LastFlush/Sealed/TablesSent
	FlushMu       sync.Mutex // serializes flush I/O for this session
	Msgs          map[string]*StreamEntry
	Order         []string
	Stopped       bool // turn closed; drop late deltas
	StopRequested bool
	// LastStopBody is the trimmed authoritative Stop body (set by FinalizeLastWithText). Used for
	// content-dedup of a late MessageDisplay after Stop: a post-Stop MD whose content equals this was
	// already delivered by the Stop send (incl. the round-7 SealedMismatch direct-send), so it is dropped.
	LastStopBody string
	// NewTextSinceTool is set when any assistant text arrives since the last tool (a new bubble AND a
	// continuation delta — both set sites). The PreToolUse FIFO handler consumes it (TakeNewTextSinceTool) to
	// gate ONLY the pre-tool wait-skip (f25: it no longer drives the compact-tool cycle reset — that now keys on
	// SendBelowSinceTool, the placement signal). Guarded by DataMu.
	NewTextSinceTool bool
	// SendBelowSinceTool is set (f25) when a flush places (or will place at the pre-tool flush) a NEW TG text
	// message BELOW the last tool notification since the last tool boundary — a new bubble or pagination growth
	// (planned chunks > PositionedChunks). The PreToolUse FIFO handler consumes it (TakeSendBelowSinceTool) to
	// reset the compact-tool cycle, so a continuation that only EDITS the bubble above the tool no longer falsely
	// splits the compact group (rev 14 BLOCKER3 side effect). Set in flushSession p3 under DataMu; INV4-safe
	// (the Hook FIFO reads this flag, never len(Msgs)).
	SendBelowSinceTool bool
	Closed             bool      // session ended/reset; in-flight flush must abort + AppendDelta drops
	ClosedTurns        []string  // turn_ids whose stream is finalized; their late deltas are dropped (survives Rotate; capped)
	EndedAt            time.Time // when SessionEnd marked this a tombstone (for TTL sweep)
	// Round-8 async-race coordination, folded here (Occam — no separate store). Guarded by DataMu.
	PostToolArrived map[string]*PostToolSignal // tool_use_id -> PostToolUse payload, set off-FIFO before Dispatch (B2)
	LastToolNotify  ToolNotifyMark             // last B2/B3-resolved tool notification (late-MD observability marker)
}

type StreamStore struct {
	mu       sync.Mutex
	sessions map[string]*SessionStream
}

func NewStreamStore() *StreamStore { return &StreamStore{sessions: make(map[string]*SessionStream)} }

func (s *StreamStore) Session(sessionID string) *SessionStream {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss, ok := s.sessions[sessionID]
	if !ok {
		ss = &SessionStream{Msgs: make(map[string]*StreamEntry)}
		s.sessions[sessionID] = ss
	}
	return ss
}

// AppendDelta records one delta under DataMu only (never blocks on I/O). Returns isNew.
func (s *StreamStore) AppendDelta(sessionID string, m StreamMeta, index int, delta string, final bool) bool {
	ss := s.Session(sessionID)
	ss.DataMu.Lock()
	defer ss.DataMu.Unlock()
	if ss.Closed || ss.Stopped || turnClosed(ss, m.TurnID) {
		return false // barrier: session ended / turn stopped / turn finalized — drop late deltas
	}
	e, ok := ss.Msgs[m.MessageID]
	if ok && e.Sealed {
		return false // ignore late/duplicate delta for an already-finalized message
	}
	isNew := false
	if !ok {
		e = &StreamEntry{MessageID: m.MessageID, TurnID: m.TurnID, PromptID: m.PromptID, ChatID: m.ChatID, TopicID: m.TopicID,
			TmuxTarget: m.TmuxTarget, Project: m.Project, CWD: m.CWD, AgentName: m.AgentName,
			Backend: m.Backend, Deltas: make(map[int]string), FinalIdx: -1}
		ss.Msgs[m.MessageID] = e
		ss.Order = append(ss.Order, m.MessageID)
		isNew = true
	}
	e.Deltas[index] = delta
	if final {
		e.FinalIdx = index
	}
	e.Dirty = true
	if isNew {
		ss.NewTextSinceTool = true
	} // a new assistant text bubble started since the last tool
	return isNew
}

// TakeNewTextSinceTool atomically reads+clears the per-session NewTextSinceTool flag (the PreToolUse FIFO
// consumes it to gate the pre-tool wait-skip; f25: it no longer drives the compact-tool cycle reset — see
// TakeSendBelowSinceTool). False if unknown.
func (s *StreamStore) TakeNewTextSinceTool(sessionID string) bool {
	ss := s.lookup(sessionID)
	if ss == nil {
		return false
	}
	ss.DataMu.Lock()
	defer ss.DataMu.Unlock()
	v := ss.NewTextSinceTool
	ss.NewTextSinceTool = false
	return v
}

// TakeSendBelowSinceTool atomically reads+clears the per-session SendBelowSinceTool placement signal (f25):
// the PreToolUse FIFO consumes it AFTER the pre-tool flush to reset the compact-tool cycle iff a new TG text
// message was (or will be) placed below the last tool notification since the last tool. False if unknown.
func (s *StreamStore) TakeSendBelowSinceTool(sessionID string) bool {
	ss := s.lookup(sessionID)
	if ss == nil {
		return false
	}
	ss.DataMu.Lock()
	defer ss.DataMu.Unlock()
	v := ss.SendBelowSinceTool
	ss.SendBelowSinceTool = false
	return v
}

// turnClosed reports whether turnID is in the (capped) closed-turn tombstone list. Caller holds DataMu.
func turnClosed(ss *SessionStream, turnID string) bool {
	if turnID == "" {
		return false
	}
	for _, t := range ss.ClosedTurns {
		if t == turnID {
			return true
		}
	}
	return false
}

// recordClosedTurns adds the turn_ids of the current entries to the tombstone (deduped, cap 8). Caller holds DataMu.
func recordClosedTurns(ss *SessionStream) {
	for _, e := range ss.Msgs {
		if e.TurnID == "" || turnClosed(ss, e.TurnID) {
			continue
		}
		ss.ClosedTurns = append(ss.ClosedTurns, e.TurnID)
	}
	if n := len(ss.ClosedTurns); n > 8 {
		ss.ClosedTurns = ss.ClosedTurns[n-8:]
	}
}

// Post-Stop late-MessageDisplay design (commit 18) — residuals (accepted tradeoffs):
//   (a) a NEW message whose first non-empty fragment verbatim-matches a paragraph run of the authoritative
//       Stop body is classified STOP-COPY and dropped WHOLE (boss-accepted homogeneity tradeoff — dedup wins
//       over the rare case of a genuinely-new message that happens to start with the Stop body verbatim).
//   (b) no cross-turn tombstone is recorded if no late MD of that turn arrives before Rotate (maybeTombstone
//       fires only on observation in the stopped handler).
//   (c) incomplete late entries are cleared at Rotate (existing lifecycle — Rotate wipes ss.Msgs/ss.Order).
//
// PostStopAction tells the MessageDisplay handler what follow-up (if any) a delta requires while the session
// is Stopped (commit 18). The non-stopped path always returns PostStopNone.
type PostStopAction int

const (
	PostStopNone          PostStopAction = iota // not a post-Stop delta — use (handled, dropped) normally
	PostStopDrop                                // classified STOP-COPY → dropped whole (INFO "dropped (stop-copy)")
	PostStopDefer                               // empty delta on an unclassified placeholder → nothing to render yet
	PostStopNeedsArm                            // a NEW entry gained content → caller ResolveChats then ArmStopped
	PostStopArmedComplete                       // an ARMED entry became complete → caller StreamFlush(sessionID, true)
	PostStopArmedProgress                       // an ARMED entry took a non-final delta → the ticker renders it
)

// AppendExisting appends a delta WITHOUT chat resolution when the (session,message) entry already exists.
// Returns handled=true if handled (appended OR dropped OR routed by the post-Stop state machine); handled=false
// only when the message is brand-new AND the session is NOT stopped, in which case the caller resolves the chat
// and calls AppendDelta. Keeps ResolveChat off the per-delta hot path.
//
// Check order (P3, commit 18): ss.Closed first (unconditional drop) → the post-Stop state machine while
// ss.Stopped → turnClosed ONLY when NOT stopped. While Stopped, a late MessageDisplay is redesigned from a
// one-shot direct send into a proper order-gated stream (see stoppedMachine); the returned PostStopAction
// drives the caller's follow-up (arm / flush / drop).
func (s *StreamStore) AppendExisting(sessionID, messageID, turnID string, index int, delta string, final bool) (handled, dropped bool, post PostStopAction) {
	ss := s.lookup(sessionID)
	if ss == nil {
		return false, false, PostStopNone
	}
	ss.DataMu.Lock()
	defer ss.DataMu.Unlock()
	if ss.Closed {
		return true, true, PostStopNone // ended — unconditional safe drop
	}
	if ss.Stopped {
		return stoppedMachine(ss, messageID, turnID, index, delta, final)
	}
	if turnClosed(ss, turnID) {
		return true, true, PostStopNone // finalized turn — safe drop (checked only when NOT stopped)
	}
	e, ok := ss.Msgs[messageID]
	if !ok {
		return false, false, PostStopNone // new message → caller resolves chat + AppendDelta
	}
	if e.Sealed {
		return true, true, PostStopNone // message already finalized
	}
	e.Deltas[index] = delta
	if final {
		e.FinalIdx = index
	}
	e.Dirty = true
	// A continuation delta on an existing bubble IS assistant text since the last tool (rev 14 BLOCKER3): set
	// NewTextSinceTool so the pre-tool wait-skip triggers for continuation text, not just a fresh bubble. Only
	// on the appended path — never on the closed/sealed drop paths above.
	ss.NewTextSinceTool = true
	return true, false, PostStopNone
}

// stoppedMachine routes a MessageDisplay delta that arrives while the session is Stopped (commit 18). Runs
// under the caller's ss.DataMu hold (DataMu-only: NO ResolveChat, NO I/O):
//   - known & Sealed              → drop (late/duplicate on an armed-then-sealed or pre-Stop sealed entry).
//   - armed (in ss.Order) & unsealed → append; ArmedComplete if now complete, else ArmedProgress (P4/P5).
//   - unknown / unarmed placeholder → PER-MESSAGE STICKY CLASSIFICATION (P1) on the first NON-EMPTY delta:
//       STOP-COPY (a paragraph run of LastStopBody) → drop every delta, never arm (no storage);
//       NEW → store the delta and request arming once the assembled text is non-empty (P2);
//       empty/whitespace deltas defer (record index/FinalIdx continuity only, never classify, never Dirty).
//
// On the first observation of a late turn_id it records the turn tombstone (P6, recordClosedTurns semantics).
func stoppedMachine(ss *SessionStream, messageID, turnID string, index int, delta string, final bool) (handled, dropped bool, post PostStopAction) {
	e, known := ss.Msgs[messageID]
	if known && e.Sealed {
		maybeTombstone(ss, turnID)
		return true, true, PostStopNone // known-sealed straggler → existing "dropped" log
	}
	if known && isInOrder(ss, messageID) {
		// Armed entry (a NEW post-Stop stream already armed, or a pre-Stop unsealed bubble): append + flush on
		// completion (P4/P5). The ticker renders non-final progress (order-gated).
		e.Deltas[index] = delta
		if final {
			e.FinalIdx = index
		}
		e.Dirty = true
		maybeTombstone(ss, turnID)
		if _, complete := e.AssembledText(); complete {
			return true, false, PostStopArmedComplete
		}
		return true, false, PostStopArmedProgress
	}
	// Unknown message_id, or a known-but-UNARMED placeholder (in ss.Msgs, ABSENT from ss.Order — P2).
	if !known {
		e = &StreamEntry{MessageID: messageID, TurnID: turnID, Deltas: make(map[int]string), FinalIdx: -1, StopClass: StopClassUnclassified}
		ss.Msgs[messageID] = e
	}
	maybeTombstone(ss, turnID)
	if e.StopClass == StopClassStopCopy {
		return true, true, PostStopDrop // sticky: every delta of a STOP-COPY id is dropped, no storage
	}
	if e.StopClass == StopClassUnclassified {
		if strings.TrimSpace(delta) == "" {
			// Empty/whitespace delta: record index/FinalIdx continuity ONLY (never classify, never Dirty). An
			// all-empty message stays a permanent no-op.
			e.Deltas[index] = delta
			if final {
				e.FinalIdx = index
			}
			return true, false, PostStopDefer
		}
		// First NON-EMPTY delta → classify exactly once (P1).
		if paragraphRunContained(delta, ss.LastStopBody) {
			e.StopClass = StopClassStopCopy // verbatim Stop-body run → drop this and every later delta (no storage)
			return true, true, PostStopDrop
		}
		e.StopClass = StopClassNew
	}
	// StopClassNew (just classified or continuing an unarmed NEW entry): store the delta; request arming once
	// the assembled text is non-empty (P2). Do NOT set Dirty — an UNARMED entry is never rendered (order-gated),
	// so it never wakes the ticker; ArmStopped sets Dirty at arm time.
	e.Deltas[index] = delta
	if final {
		e.FinalIdx = index
	}
	if text, _ := e.AssembledText(); strings.TrimSpace(text) != "" {
		return true, false, PostStopNeedsArm
	}
	return true, false, PostStopDefer
}

// maybeTombstone records the turn tombstone on the FIRST observation of a late turn_id in the stopped handler
// (P6). recordClosedTurns picks up the just-handled entry's TurnID (dedup + cap semantics), so a delta of this
// turn arriving after a later Rotate (which keeps ClosedTurns) is dropped by turnClosed.
func maybeTombstone(ss *SessionStream, turnID string) {
	if turnID != "" && !turnClosed(ss, turnID) {
		recordClosedTurns(ss)
	}
}

// isInOrder reports whether messageID is armed (present in ss.Order). Caller holds ss.DataMu.
func isInOrder(ss *SessionStream, messageID string) bool {
	for _, id := range ss.Order {
		if id == messageID {
			return true
		}
	}
	return false
}

// ArmStopped installs the FULL metadata on a post-Stop NEW entry and appends it to ss.Order exactly once (P2
// arming), all under ONE DataMu hold so no render snapshot ever sees the entry in ss.Order without its
// metadata. Returns armed=false if the entry vanished (Rotate/Close raced); complete reports whether the
// assembled text is already final (a single-delta message → the caller flushes immediately, P5).
func (s *StreamStore) ArmStopped(sessionID, messageID string, m StreamMeta) (armed, complete bool) {
	ss := s.lookup(sessionID)
	if ss == nil {
		return false, false
	}
	ss.DataMu.Lock()
	defer ss.DataMu.Unlock()
	e, ok := ss.Msgs[messageID]
	if !ok {
		return false, false // entry removed between classify and arm
	}
	// Mirror the normal new-message path's metadata set (register.go new-message block) BEFORE Dirty/Order.
	e.PromptID = m.PromptID
	e.ChatID = m.ChatID
	e.TopicID = m.TopicID
	e.TmuxTarget = m.TmuxTarget
	e.Project = m.Project
	e.CWD = m.CWD
	e.AgentName = m.AgentName
	e.Backend = m.Backend
	if !isInOrder(ss, messageID) {
		ss.Order = append(ss.Order, messageID)
	}
	e.Dirty = true
	_, complete = e.AssembledText()
	return true, complete
}

// DropStopped removes a post-Stop entry that could not be armed (ResolveChat failed) — deletes the map entry
// and defensively sweeps ss.Order (P2), so no unarmed, metadata-less entry lingers.
func (s *StreamStore) DropStopped(sessionID, messageID string) {
	ss := s.lookup(sessionID)
	if ss == nil {
		return
	}
	ss.DataMu.Lock()
	defer ss.DataMu.Unlock()
	delete(ss.Msgs, messageID)
	newOrder := ss.Order[:0]
	for _, id := range ss.Order {
		if id != messageID {
			newOrder = append(newOrder, id)
		}
	}
	ss.Order = newOrder
}

// splitParagraphs normalizes CRLF/lone-CR to LF, then splits on blank-line boundaries into TrimSpace'd
// paragraphs (empty input → nil). Used by the post-Stop STOP-COPY matcher (P1).
func splitParagraphs(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var paras []string
	var cur []string
	flush := func() {
		if len(cur) > 0 {
			if p := strings.TrimSpace(strings.Join(cur, "\n")); p != "" {
				paras = append(paras, p)
			}
			cur = nil
		}
	}
	for _, ln := range strings.Split(s, "\n") {
		if strings.TrimSpace(ln) == "" {
			flush()
		} else {
			cur = append(cur, ln)
		}
	}
	flush()
	return paras
}

// paragraphRunContained reports whether inner is a CONTIGUOUS paragraph run of outer (P1 matcher). Both are
// normalized + split by splitParagraphs; empty inner or outer → false.
func paragraphRunContained(inner, outer string) bool {
	ip := splitParagraphs(inner)
	op := splitParagraphs(outer)
	if len(ip) == 0 || len(op) == 0 || len(ip) > len(op) {
		return false
	}
	for start := 0; start+len(ip) <= len(op); start++ {
		match := true
		for k := range ip {
			if ip[k] != op[start+k] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// Rotate clears the current turn's entries for a NEW user turn but KEEPS the ClosedTurns tombstone,
// so a late delta from the previous (closed) turn is still dropped. Used by UserPromptSubmit.
// DataMu-ONLY (S3a): Rotate runs INSIDE a Message-FIFO op (INV6 lifecycle-as-op) so it is already
// totally ordered with every render op — it must NOT take FlushMu (no Message-FIFO op holds FlushMu).
func (s *StreamStore) Rotate(sessionID string) {
	ss := s.lookup(sessionID)
	if ss == nil {
		return
	}
	ss.DataMu.Lock()
	defer ss.DataMu.Unlock()
	ss.Msgs = make(map[string]*StreamEntry)
	ss.Order = nil
	ss.Stopped = false
	ss.StopRequested = false
	ss.LastStopBody = ""          // new user turn — drop the previous turn's authoritative Stop body
	ss.NewTextSinceTool = false   // new user turn — drop any stale "new text since last tool" signal
	ss.SendBelowSinceTool = false // f25: new user turn — drop any stale placement (send-below) signal
}

// lookup returns the existing SessionStream without creating one.
func (s *StreamStore) lookup(sessionID string) *SessionStream {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[sessionID]
}

// LastStatus reports whether the session has any message entry and whether its LAST one is complete (for drain).
func (s *StreamStore) LastStatus(sessionID string) (has, complete bool) {
	ss := s.lookup(sessionID)
	if ss == nil {
		return false, false
	}
	ss.DataMu.Lock()
	defer ss.DataMu.Unlock()
	if len(ss.Order) == 0 {
		return false, false
	}
	e := ss.Msgs[ss.Order[len(ss.Order)-1]]
	_, c := e.AssembledText()
	return true, c
}

// CompleteCount returns how many message bubbles are fully assembled (final delta received + contiguous).
// The AskUserQuestion drain captures this at entry and waits for it to rise (a NEW text bubble completed).
func (s *StreamStore) CompleteCount(sessionID string) int {
	ss := s.lookup(sessionID)
	if ss == nil {
		return 0
	}
	ss.DataMu.Lock()
	defer ss.DataMu.Unlock()
	n := 0
	for _, e := range ss.Msgs {
		if _, c := e.AssembledText(); c {
			n++
		}
	}
	return n
}

func (s *StreamStore) MarkStopRequested(sessionID string) {
	ss := s.Session(sessionID)
	ss.DataMu.Lock()
	ss.StopRequested = true
	ss.DataMu.Unlock()
}
func (s *StreamStore) MarkStopped(sessionID string) {
	ss := s.Session(sessionID)
	ss.DataMu.Lock()
	ss.Stopped = true
	recordClosedTurns(ss)
	ss.DataMu.Unlock()
}

// TryClose closes the turn (Stopped=true) ONLY if quiescent (no dirty entry and the last message is complete),
// atomically under DataMu so a delta cannot slip in between the check and the close. Returns true if closed.
func (s *StreamStore) TryClose(sessionID string) bool {
	ss := s.lookup(sessionID)
	if ss == nil {
		return true
	}
	ss.DataMu.Lock()
	defer ss.DataMu.Unlock()
	for _, e := range ss.Msgs {
		if e.Dirty {
			return false
		}
		if _, c := e.AssembledText(); !c {
			return false
		}
	}
	ss.Stopped = true
	recordClosedTurns(ss)
	return true
}

// FinalizeResult reports what FinalizeLastWithText did with the authoritative Stop text.
type FinalizeResult int

const (
	FinalizeSkipped        FinalizeResult = iota // empty text, or the sealed last entry already contains the Stop text
	FinalizeExisting                             // the last entry was overwritten with the authoritative text and sealed
	FinalizeNoEntry                              // no stream entry yet (Stop raced ahead of the last MessageDisplay)
	FinalizeSealedMismatch                       // last entry is sealed but does NOT contain the Stop authoritative text
)

// streamGracePoll is the poll interval AwaitEntryOrStop uses while grace-waiting for a late MessageDisplay.
const streamGracePoll = 25 * time.Millisecond

// FinalizeLastWithText installs the Stop-hook's authoritative last_assistant_message as the LAST stream
// entry's content, so the Stop flush renders the COMPLETE text instead of force-sealing a partial
// MessageDisplay stream. Under one DataMu hold:
//   - empty/whitespace text     -> FinalizeSkipped (no change).
//   - no entry (Stop beat the last MD) -> FinalizeNoEntry with NO side effect; the caller uses
//     AwaitEntryOrStop to grace-wait for a near-concurrent MD before deciding (do NOT mark Stopped here —
//     an instant stop would drop a slightly-late MD that the old streamFlush drain grace used to catch).
//   - last entry already Sealed AND its assembled text equals the Stop text -> FinalizeSkipped (MD already
//     completed it correctly; do not overwrite). Sealed but assembled text DIFFERS (an earlier message whose
//     post-tool text raced Stop and was dropped) -> FinalizeSealedMismatch (the caller direct-sends the Stop text).
//   - otherwise -> overwrite the last entry's Deltas with {0: text}, FinalIdx=0, Sealed=true (the seal
//     makes AppendDelta/AppendExisting drop any late/out-of-order MD, incl. an index=0 overwrite), and
//     return FinalizeExisting.
func (s *StreamStore) FinalizeLastWithText(sessionID, text string) FinalizeResult {
	if strings.TrimSpace(text) == "" {
		return FinalizeSkipped
	}
	ss := s.Session(sessionID)
	ss.DataMu.Lock()
	defer ss.DataMu.Unlock()
	// Record the authoritative Stop body (trimmed) for content-dedup of a late post-Stop MessageDisplay,
	// for ALL non-empty outcomes (Existing/Skipped/SealedMismatch/NoEntry). text is non-empty here (the
	// empty-text early return above already returned FinalizeSkipped).
	ss.LastStopBody = strings.TrimSpace(text)
	if len(ss.Order) == 0 {
		return FinalizeNoEntry
	}
	e := ss.Msgs[ss.Order[len(ss.Order)-1]]
	if e == nil {
		return FinalizeSkipped
	}
	if e.Sealed {
		// The last entry is already sealed. If it already carries the Stop authoritative text, the MD
		// completed it correctly -> skip. If it carries a DIFFERENT (earlier) message (e.g. a pre-tool text
		// bubble, and the post-tool text raced Stop and was dropped), the Stop text was never delivered ->
		// FinalizeSealedMismatch so the caller direct-sends it.
		assembled, _ := e.AssembledText()
		if strings.TrimSpace(assembled) == strings.TrimSpace(text) {
			return FinalizeSkipped
		}
		return FinalizeSealedMismatch
	}
	e.Deltas = map[int]string{0: text}
	e.FinalIdx = 0
	e.Sealed = true
	e.Dirty = true
	return FinalizeExisting
}

// AwaitEntryOrStop waits up to timeout for a stream entry to appear (a MessageDisplay arriving just after
// Stop won the race). It returns true as soon as an entry exists — the caller should install the
// authoritative text and flush (relabel). If the timeout elapses with still no entry, it ATOMICALLY (under
// the SAME DataMu hold as the final len(Order) check) marks the session Stopped so a later MD is dropped,
// and returns false — the caller direct-sends the payload. This restores the grace the old streamFlush
// drain gave a slightly-late MD, so the common near-concurrent case relabels instead of being dropped.
func (s *StreamStore) AwaitEntryOrStop(sessionID string, timeout time.Duration) bool {
	ss := s.Session(sessionID)
	deadline := time.Now().Add(timeout)
	for {
		ss.DataMu.Lock()
		if len(ss.Order) > 0 {
			ss.DataMu.Unlock()
			return true
		}
		if !time.Now().Before(deadline) {
			// Final len(Order) check (empty, above) + Stopped commit under ONE lock hold — no split acquisition.
			ss.Stopped = true
			recordClosedTurns(ss)
			ss.DataMu.Unlock()
			return false
		}
		ss.DataMu.Unlock()
		time.Sleep(streamGracePoll)
	}
}

// DueSessions returns sessions with a dirty entry that is complete OR past the throttle.
func (s *StreamStore) DueSessions(throttle time.Duration) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	now := time.Now()
	for sid, ss := range s.sessions {
		ss.DataMu.Lock()
		for _, e := range ss.Msgs {
			if !e.Dirty {
				continue
			}
			if _, c := e.AssembledText(); c || now.Sub(e.LastFlush) >= throttle {
				out = append(out, sid)
				break
			}
		}
		ss.DataMu.Unlock()
	}
	return out
}

// EndSession turns the session into a TTL tombstone (Closed=true, entries cleared) instead of deleting it,
// so a late async delta after SessionEnd cannot resurrect a fresh stream and post a stray 💬.
// DataMu-ONLY (S3a): EndSession runs INSIDE a Message-FIFO op (INV6 lifecycle-as-op), totally ordered with
// every render op, so it must NOT take FlushMu (no Message-FIFO op holds FlushMu).
func (s *StreamStore) EndSession(sessionID string) {
	ss := s.Session(sessionID) // create a bare tombstone if none exists, so a straggler delta is still dropped
	ss.DataMu.Lock()
	defer ss.DataMu.Unlock()
	recordClosedTurns(ss)
	ss.Msgs = make(map[string]*StreamEntry)
	ss.Order = nil
	ss.Closed = true
	ss.Stopped = true
	ss.SendBelowSinceTool = false // f25: session ended — drop any stale placement (send-below) signal
	ss.EndedAt = time.Now()
}

// SweepEnded deletes ended-session tombstones older than ttl (called periodically by the worker).
func (s *StreamStore) SweepEnded(ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for sid, ss := range s.sessions {
		ss.DataMu.Lock()
		expired := ss.Closed && !ss.EndedAt.IsZero() && now.Sub(ss.EndedAt) > ttl
		ss.DataMu.Unlock()
		if expired {
			delete(s.sessions, sid)
		}
	}
}

// SetPostToolArrived records a PostToolUse/PostToolUseFailure payload keyed by tool_use_id. Called off-FIFO
// from the /hook/ HTTP handler BEFORE Dispatch, so the (possibly blocked) PreToolUse wait can observe it.
func (s *StreamStore) SetPostToolArrived(sessionID, toolUseID, toolName string, resp json.RawMessage) {
	if sessionID == "" || toolUseID == "" {
		return
	}
	ss := s.Session(sessionID)
	ss.DataMu.Lock()
	defer ss.DataMu.Unlock()
	if ss.PostToolArrived == nil {
		ss.PostToolArrived = make(map[string]*PostToolSignal)
	}
	ss.PostToolArrived[toolUseID] = &PostToolSignal{ToolName: toolName, Response: resp, At: time.Now()}
}

// GetPostToolArrived returns the buffered PostToolUse signal for a tool_use_id, if present.
func (s *StreamStore) GetPostToolArrived(sessionID, toolUseID string) (*PostToolSignal, bool) {
	ss := s.lookup(sessionID)
	if ss == nil {
		return nil, false
	}
	ss.DataMu.Lock()
	defer ss.DataMu.Unlock()
	sig, ok := ss.PostToolArrived[toolUseID]
	return sig, ok
}

// ConsumePostToolArrived removes a buffered PostToolUse signal (after it has been applied inline).
func (s *StreamStore) ConsumePostToolArrived(sessionID, toolUseID string) {
	ss := s.lookup(sessionID)
	if ss == nil {
		return
	}
	ss.DataMu.Lock()
	defer ss.DataMu.Unlock()
	delete(ss.PostToolArrived, toolUseID)
}

// SweepPostToolArrived drops PostToolUse signals older than ttl (never-consumed cases: AskUserQuestion PTU
// skips the wait, subagent PTU breaks early — neither consumes the signal).
func (s *StreamStore) SweepPostToolArrived(ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for _, ss := range s.sessions {
		ss.DataMu.Lock()
		for id, sig := range ss.PostToolArrived {
			if now.Sub(sig.At) > ttl {
				delete(ss.PostToolArrived, id)
			}
		}
		ss.DataMu.Unlock()
	}
}

// SetLastToolNotify records the last tool notification send for the late-MD observability marker.
func (s *StreamStore) SetLastToolNotify(sessionID, promptID string) {
	if sessionID == "" {
		return
	}
	ss := s.Session(sessionID)
	ss.DataMu.Lock()
	defer ss.DataMu.Unlock()
	ss.LastToolNotify = ToolNotifyMark{PromptID: promptID, At: time.Now()}
}

// GetLastToolNotify returns the last recorded tool-notify mark (ok=false if none recorded yet).
func (s *StreamStore) GetLastToolNotify(sessionID string) (ToolNotifyMark, bool) {
	ss := s.lookup(sessionID)
	if ss == nil {
		return ToolNotifyMark{}, false
	}
	ss.DataMu.Lock()
	defer ss.DataMu.Unlock()
	if ss.LastToolNotify.At.IsZero() {
		return ToolNotifyMark{}, false
	}
	return ss.LastToolNotify, true
}
