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
// Deltas/FinalIdx/Dirty/LastFlush are guarded by SessionStream.DataMu.
// Msgs/SentText/TablesSent/Sealed are mutated only by flush (serialized by SessionStream.FlushMu).
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
	Dirty      bool
	LastFlush  time.Time
}

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

// ToolNotifyMark records the last PreToolUse tool-notification send that resolved via B2/B3, so a same-prompt
// MessageDisplay-final arriving shortly after can be logged as a residual inversion (observability only).
type ToolNotifyMark struct {
	PromptID string
	At       time.Time
	Branch   string
}

type SessionStream struct {
	DataMu        sync.Mutex // guards Msgs/Order/Stopped/Closed + per-entry Deltas/FinalIdx/Dirty/LastFlush/Sealed/TablesSent
	FlushMu       sync.Mutex // serializes flush I/O for this session
	Msgs          map[string]*StreamEntry
	Order         []string
	Stopped       bool // turn closed; drop late deltas
	StopRequested bool
	// NewTextSinceTool is set when a new assistant text bubble (message_id) starts; the PreToolUse FIFO
	// handler consumes it (TakeNewTextSinceTool) to reset the compact-tool cycle, keeping Reset serialized
	// with CompactTools Get/Store on the per-session worker (fixes the off-FIFO Reset-vs-Store race).
	NewTextSinceTool bool
	Closed           bool      // session ended/reset; in-flight flush must abort + AppendDelta drops
	ClosedTurns      []string  // turn_ids whose stream is finalized; their late deltas are dropped (survives Rotate; capped)
	EndedAt          time.Time // when SessionEnd marked this a tombstone (for TTL sweep)
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
// consumes it to reset the compact-tool cycle, serialized with CompactTools Get/Store). False if unknown.
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

// AppendExisting appends a delta WITHOUT chat resolution when the (session,message) entry already exists.
// Returns true if handled (appended OR dropped as closed/sealed); false only when the message is brand-new,
// in which case the caller resolves the chat and calls AppendDelta. Keeps ResolveChat off the per-delta hot path.
func (s *StreamStore) AppendExisting(sessionID, messageID, turnID string, index int, delta string, final bool) (handled, dropped bool) {
	ss := s.lookup(sessionID)
	if ss == nil {
		return false, false
	}
	ss.DataMu.Lock()
	defer ss.DataMu.Unlock()
	if ss.Closed || ss.Stopped || turnClosed(ss, turnID) {
		return true, true
	} // dropped: ended/stopped/closed turn
	e, ok := ss.Msgs[messageID]
	if !ok {
		return false, false
	} // new message → caller resolves chat + AppendDelta
	if e.Sealed {
		return true, true
	} // dropped: message already finalized
	e.Deltas[index] = delta
	if final {
		e.FinalIdx = index
	}
	e.Dirty = true
	return true, false
}

// Rotate clears the current turn's entries for a NEW user turn but KEEPS the ClosedTurns tombstone,
// so a late delta from the previous (closed) turn is still dropped. Used by UserPromptSubmit.
func (s *StreamStore) Rotate(sessionID string) {
	ss := s.lookup(sessionID)
	if ss == nil {
		return
	}
	ss.FlushMu.Lock()
	defer ss.FlushMu.Unlock()
	ss.DataMu.Lock()
	defer ss.DataMu.Unlock()
	ss.Msgs = make(map[string]*StreamEntry)
	ss.Order = nil
	ss.Stopped = false
	ss.StopRequested = false
	ss.NewTextSinceTool = false // new user turn — drop any stale "new text since last tool" signal
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

// Reset closes and drops the session. Acquires FlushMu so it waits for any in-flight flush, then sets
// Closed=true (so a flush that already grabbed the old pointer aborts) before deleting from the map.
func (s *StreamStore) Reset(sessionID string) {
	ss := s.lookup(sessionID)
	if ss != nil {
		ss.FlushMu.Lock()
		ss.DataMu.Lock()
		ss.Closed = true
		ss.DataMu.Unlock()
		ss.FlushMu.Unlock()
	}
	s.mu.Lock()
	delete(s.sessions, sessionID)
	s.mu.Unlock()
}

// EndSession turns the session into a TTL tombstone (Closed=true, entries cleared) instead of deleting it,
// so a late async delta after SessionEnd cannot resurrect a fresh stream and post a stray 💬.
func (s *StreamStore) EndSession(sessionID string) {
	ss := s.Session(sessionID) // create a bare tombstone if none exists, so a straggler delta is still dropped
	ss.FlushMu.Lock()
	defer ss.FlushMu.Unlock()
	ss.DataMu.Lock()
	defer ss.DataMu.Unlock()
	recordClosedTurns(ss)
	ss.Msgs = make(map[string]*StreamEntry)
	ss.Order = nil
	ss.Closed = true
	ss.Stopped = true
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

// HasCompleteUnsealedForPrompt reports whether some bubble tagged with promptID is fully assembled (final
// delta received) but NOT yet flushed/sealed — the B1 condition for the PreToolUse bounded wait. Sealed
// bubbles are excluded so a prior sealed segment of a multi-segment turn does not falsely satisfy the wait
// for a later tool whose preceding text is still in flight (advisor MAJOR 2b/2c).
func (s *StreamStore) HasCompleteUnsealedForPrompt(sessionID, promptID string) bool {
	if promptID == "" {
		return false
	}
	ss := s.lookup(sessionID)
	if ss == nil {
		return false
	}
	ss.DataMu.Lock()
	defer ss.DataMu.Unlock()
	for _, e := range ss.Msgs {
		if e.PromptID != promptID || e.Sealed {
			continue
		}
		if _, c := e.AssembledText(); c {
			return true
		}
	}
	return false
}

// HasInflightForPrompt reports whether some bubble tagged with promptID has received at least one delta but
// is NOT yet complete — i.e. pre-tool text for this prompt is still streaming. "Complete" uses the SAME
// contiguity semantics as AssembledText: a final delta (final:true) whose index leaves an earlier gap (deltas
// not contiguous from index 0) is NOT complete and therefore still counts as in-flight. Sealed bubbles are
// excluded (already flushed). This is the B2 in-flight gate for the PreToolUse bounded wait: while pre-tool
// text is in flight, keep waiting for its final (B1) up to the full budget instead of firing the tool
// notification early at the 500ms floor.
func (s *StreamStore) HasInflightForPrompt(sessionID, promptID string) bool {
	if promptID == "" {
		return false
	}
	ss := s.lookup(sessionID)
	if ss == nil {
		return false
	}
	ss.DataMu.Lock()
	defer ss.DataMu.Unlock()
	for _, e := range ss.Msgs {
		if e.PromptID != promptID || e.Sealed || len(e.Deltas) == 0 {
			continue
		}
		if _, c := e.AssembledText(); !c {
			return true
		}
	}
	return false
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

// SetLastToolNotify records the last B2/B3-resolved tool notification for the late-MD observability marker.
func (s *StreamStore) SetLastToolNotify(sessionID, promptID, branch string) {
	if sessionID == "" {
		return
	}
	ss := s.Session(sessionID)
	ss.DataMu.Lock()
	defer ss.DataMu.Unlock()
	ss.LastToolNotify = ToolNotifyMark{PromptID: promptID, At: time.Now(), Branch: branch}
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
