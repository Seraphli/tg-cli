package stores

import (
	"strings"
	"sync"
	"time"

	"github.com/Seraphli/tg-cli/internal/logger"
)

// sessionQueueCap is the SOFT cap on queued (not-yet-started) jobs per session worker. Appends block
// (Dispatch/DispatchAsync) or fail (TryDispatchAsync) at the cap, EXCEPT one bounded over-cap slot for a
// single precise-eligible MessageDisplay job while the worker awaits a lagging pre-tool MD (rev 15 MAJOR2).
const sessionQueueCap = 128

// transcriptPollInterval is how often ResolveContentAddressedToolBoundary re-reads the transcript while the
// tool_use line has not been written yet. This phase of the wait has NO queue-broadcast event to wake on — a
// transcript file write is invisible to w.cond (item 2 of note2's review, 2026-10-09: the ONLY Broadcast
// sources are enqueue/dequeue/extractEligibleBeforeBoundaryLocked-removal/the deadline timer/the floor timer —
// none of those fire when CC appends a line to its jsonl). Measured write-lag across 16 real CC tool calls
// (PreToolUse hook fire -> the tool_use line visible in the transcript) is 54.59-94.87ms (/tmp/probe_hook.log,
// re-derived 2026-10-09 — matches note2's cited "55-95 ms"). 20ms bounds the added latency on top of the real
// write to at most one tick in the common case while costing only ~3-5 extra tail-reads per tool call.
const transcriptPollInterval = 20 * time.Millisecond

// noTimeoutHorizon is NOT a real timeout. The boss's ruling (2026-10-09, "没等到就得一直等") is unbounded
// waiting with no fallback; this constant exists only so the SECOND phase of the wait (watching the queue for
// the matching MessageDisplay group) can reuse the EXISTING bounded-over-cap admission reservation
// (reservedLocked :107 / admitLocked :122 of this file) — which already takes a deadline — instead of adding a second,
// parallel no-deadline admission code path (Occam's razor: reuse the existing mechanism with a deadline so far
// in the future it never fires, rather than build a new one).
const noTimeoutHorizon = 24 * time.Hour

// sessionEventJob is one queued unit of work for a session worker. label/event/promptID/arrivedAt are the
// drain-lookahead metadata (event classification + absolute arrival deadline eligibility); messageID/index/
// final/delta are populated on MessageDisplay jobs only (zero-value on every other event — fix C's content-
// addressed boundary resolution, cmd C's ResolveContentAddressedToolBoundary/drainContentMatchLocked); handler
// is the work; done (when non-nil) receives the handler error for a sync Dispatch caller.
type sessionEventJob struct {
	label     string
	event     string
	promptID  string
	messageID string
	index     int
	final     bool
	delta     string
	arrivedAt time.Time
	handler   func() error
	done      chan error
}

// JobMeta is the exported drain-lookahead view of a queued job's metadata, passed to eligible/boundary
// predicates so they can be authored from other packages (e.g. cmd/hooks) without naming sessionEventJob
// or reading its unexported fields.
type JobMeta struct {
	Event     string
	PromptID  string
	MessageID string
	Index     int
	Final     bool
	Delta     string
	ArrivedAt time.Time
}

// awaitReq is the per-worker lookahead reservation published (under mu) while the PreToolUse handler waits
// for a lagging pre-tool MessageDisplay. It gates the ONE bounded over-cap admission slot (rev 15 MAJOR2).
type awaitReq struct {
	active   bool
	deadline time.Time
	promptID string
}

type sessionWorker struct {
	mu    sync.Mutex
	cond  *sync.Cond
	jobs  []*sessionEventJob
	await awaitReq
}

type SessionEventStore struct {
	mu      sync.Mutex
	workers map[string]*sessionWorker
}

func NewSessionEventStore() *SessionEventStore {
	return &SessionEventStore{
		workers: make(map[string]*sessionWorker),
	}
}

func (s *SessionEventStore) getOrCreate(sessionID string) *sessionWorker {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w, ok := s.workers[sessionID]; ok {
		return w
	}
	w := &sessionWorker{}
	w.cond = sync.NewCond(&w.mu)
	s.workers[sessionID] = w
	go s.run(sessionID, w)
	return w
}

// reservedLocked reports whether job may take the single over-cap slot given the current await. Caller
// holds w.mu. Mirrors the ADMISSION predicate (B.14): active await, MessageDisplay event, within the
// absolute arrival deadline, and prompt-compatible (a non-empty mismatch disqualifies).
func reservedLocked(w *sessionWorker, job *sessionEventJob) bool {
	if !w.await.active || job.event != "MessageDisplay" {
		return false
	}
	if job.arrivedAt.After(w.await.deadline) {
		return false
	}
	if job.promptID != "" && w.await.promptID != "" && job.promptID != w.await.promptID {
		return false
	}
	return true
}

// admitLocked reports whether job may be appended right now. Caller holds w.mu. Admits when below cap, or
// exactly at cap for a reserved (precise-eligible) MD — bounding the over-cap to ONE slot (cap+1).
func admitLocked(w *sessionWorker, job *sessionEventJob) bool {
	if len(w.jobs) < sessionQueueCap {
		return true
	}
	return reservedLocked(w, job) && len(w.jobs) == sessionQueueCap
}

// enqueueBlocking appends job, blocking until admitted (Dispatch/DispatchAsync). Broadcasts on enqueue.
func (w *sessionWorker) enqueueBlocking(job *sessionEventJob) {
	w.mu.Lock()
	for !admitLocked(w, job) {
		w.cond.Wait()
	}
	w.jobs = append(w.jobs, job)
	w.cond.Broadcast()
	w.mu.Unlock()
}

// tryEnqueue appends job iff it is admissible right now (TryDispatchAsync), returning whether it did.
func (w *sessionWorker) tryEnqueue(job *sessionEventJob) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !admitLocked(w, job) {
		return false
	}
	w.jobs = append(w.jobs, job)
	w.cond.Broadcast()
	return true
}

// dequeue pops the front job, blocking until one is available. Broadcasts on removal (wakes blocked
// appenders now that a slot freed).
func (w *sessionWorker) dequeue() *sessionEventJob {
	w.mu.Lock()
	defer w.mu.Unlock()
	for len(w.jobs) == 0 {
		w.cond.Wait()
	}
	job := w.jobs[0]
	w.jobs = w.jobs[1:]
	w.cond.Broadcast()
	return job
}

func (s *SessionEventStore) run(sessionID string, w *sessionWorker) {
	for {
		job := w.dequeue()
		err := job.handler()
		if err != nil {
			logger.Debug("SessionEventStore: session=" + sessionID + " job=" + job.label + " err=" + err.Error())
		}
		if job.done != nil {
			job.done <- err
			close(job.done)
		}
	}
}

// Dispatch enqueues a job synchronously and returns the handler's error. Blocks at the cap until admitted.
// sessionID=="" runs inline (fast path).
func (s *SessionEventStore) Dispatch(sessionID, label string, handler func() error) error {
	if sessionID == "" {
		return handler()
	}
	w := s.getOrCreate(sessionID)
	done := make(chan error, 1)
	w.enqueueBlocking(&sessionEventJob{label: label, handler: handler, done: done})
	return <-done
}

// JobInput is the drain-lookahead metadata for DispatchWithMeta, carried as one struct instead of growing
// the positional-arg list further (fix C adds MessageID/Index/Final/Delta, populated on MessageDisplay only).
type JobInput struct {
	Label     string
	Event     string
	PromptID  string
	MessageID string
	Index     int
	Final     bool
	Delta     string
	ArrivedAt time.Time
}

// DispatchWithMeta is Dispatch carrying the drain-lookahead metadata (JobInput).
func (s *SessionEventStore) DispatchWithMeta(sessionID string, in JobInput, handler func() error) error {
	if sessionID == "" {
		return handler()
	}
	w := s.getOrCreate(sessionID)
	done := make(chan error, 1)
	w.enqueueBlocking(&sessionEventJob{label: in.Label, event: in.Event, promptID: in.PromptID, messageID: in.MessageID, index: in.Index, final: in.Final, delta: in.Delta, arrivedAt: in.ArrivedAt, handler: handler, done: done})
	return <-done
}

// DispatchAsync enqueues a job, blocking at the cap until admitted, and returns without awaiting the
// handler. sessionID=="" runs inline (fast path).
func (s *SessionEventStore) DispatchAsync(sessionID, label string, handler func() error) {
	if sessionID == "" {
		handler()
		return
	}
	w := s.getOrCreate(sessionID)
	w.enqueueBlocking(&sessionEventJob{label: label, handler: handler})
}

// DispatchAsyncWithMeta is DispatchAsync carrying the drain-lookahead metadata.
func (s *SessionEventStore) DispatchAsyncWithMeta(sessionID, label, event, promptID string, arrivedAt time.Time, handler func() error) {
	if sessionID == "" {
		handler()
		return
	}
	w := s.getOrCreate(sessionID)
	w.enqueueBlocking(&sessionEventJob{label: label, event: event, promptID: promptID, arrivedAt: arrivedAt, handler: handler})
}

// TryDispatchAsync attempts a nonblocking enqueue to the session's worker queue.
// Returns true if the job was accepted, false if the queue is at the (soft) cap.
// Falls back to sessionID=="" behavior (inline run) if sessionID is empty.
func (s *SessionEventStore) TryDispatchAsync(sessionID, label string, handler func() error) bool {
	if sessionID == "" {
		handler()
		return true
	}
	w := s.getOrCreate(sessionID)
	return w.tryEnqueue(&sessionEventJob{label: label, handler: handler})
}

// TryDispatchAsyncWithMeta is TryDispatchAsync carrying the drain-lookahead metadata.
func (s *SessionEventStore) TryDispatchAsyncWithMeta(sessionID, label, event, promptID string, arrivedAt time.Time, handler func() error) bool {
	if sessionID == "" {
		handler()
		return true
	}
	w := s.getOrCreate(sessionID)
	return w.tryEnqueue(&sessionEventJob{label: label, event: event, promptID: promptID, arrivedAt: arrivedAt, handler: handler})
}

// QueueDepth returns the number of queued (not-yet-started) jobs for a session's worker, 0 if none.
// Observability only (logged at the PreToolUse bounded-wait entry).
func (s *SessionEventStore) QueueDepth(sessionID string) int {
	s.mu.Lock()
	w, ok := s.workers[sessionID]
	s.mu.Unlock()
	if !ok {
		return 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.jobs)
}

// extractEligibleBeforeBoundaryLocked front-scans jobs, STOPPING at the first boundary()==true job, and
// collects+REMOVES the eligible() jobs seen before it. Caller holds w.mu. Broadcasts if it removed any.
// hitBoundary reports whether a boundary was seen before the scan ended (drives the zero-grace terminate).
func extractEligibleBeforeBoundaryLocked(w *sessionWorker, eligible, boundary func(JobMeta) bool) (got []*sessionEventJob, hitBoundary bool) {
	kept := make([]*sessionEventJob, 0, len(w.jobs))
	i := 0
	for ; i < len(w.jobs); i++ {
		j := w.jobs[i]
		m := JobMeta{Event: j.event, PromptID: j.promptID, MessageID: j.messageID, Index: j.index, Final: j.final, Delta: j.delta, ArrivedAt: j.arrivedAt}
		if boundary(m) {
			hitBoundary = true
			break // STOP at the first boundary; everything from here stays in order
		}
		if eligible(m) {
			got = append(got, j)
			continue // remove: do not keep
		}
		kept = append(kept, j)
	}
	if len(got) == 0 {
		return got, hitBoundary // nothing removed; leave w.jobs untouched
	}
	// Append the untouched tail (boundary job and everything after it, or none if scan reached the end).
	kept = append(kept, w.jobs[i:]...)
	w.jobs = kept
	w.cond.Broadcast()
	return got, hitBoundary
}

// DrainAndRunMatching is called ONLY from a running job on the SAME worker goroutine. It extracts every
// already-queued eligible pre-tool MD before the next boundary and runs each handler NOW (delivering the
// done error to a sync caller). Returns the number drained.
func (s *SessionEventStore) DrainAndRunMatching(sessionID string, eligible, boundary func(JobMeta) bool) int {
	if sessionID == "" {
		return 0
	}
	w := s.getOrCreate(sessionID)
	w.mu.Lock()
	got, _ := extractEligibleBeforeBoundaryLocked(w, eligible, boundary)
	w.mu.Unlock()
	for _, j := range got {
		err := j.handler()
		if j.done != nil {
			j.done <- err
			close(j.done)
		}
	}
	return len(got)
}

// WaitForMatchOrDeadline is the no-floor / no-early-resolve wait: a queued boundary resolves it immediately
// (zero grace) and it otherwise drains lagging eligible MD until the absolute deadline. Thin wrapper over
// WaitForMatchOrDeadlineFloored with a zero floor and a nil resolve predicate — i.e. the pre-f23 behavior.
// Kept for callers/tests that do not need the b2Floor / CompleteCount-resolve extensions.
func (s *SessionEventStore) WaitForMatchOrDeadline(sessionID, ptuPromptID string, eligible, boundary func(JobMeta) bool, deadline time.Time) {
	s.WaitForMatchOrDeadlineFloored(sessionID, ptuPromptID, eligible, boundary, nil, time.Time{}, deadline)
}

// WaitForMatchOrDeadlineFloored publishes an await reservation and drains lagging eligible MD jobs on THIS
// worker until it resolves, returning a branch label for observability. Resolution (whichever comes first):
//   - resolve!=nil && resolve() becomes true — checked after each drain with mu RELEASED (so the closure may
//     take another lock without nesting under w.mu). This is the AskQ boundary: a NEW completed text bubble
//     (CompleteCount rose above the entry baseline). branch="resolved".
//   - a queued boundary, but NOT before the floor (b2Floor): hitBoundary is honored only once now>=floor, so
//     a lagging pre-tool MD still has until the floor to land and win (drain) first. branch="boundary_floored".
//   - the absolute deadline — the budget / B3 ceiling. branch="deadline".
//
// An eligible MD is drained at ANY time (incl. inside the floor); draining is prompt (early Broadcast wake)
// but does NOT itself short-circuit the wait unless resolve() says so — matching the pre-f23 tool-boundary
// contract. floor==zero disables the floor (a queued boundary resolves immediately); resolve==nil disables
// early resolution. Explicit lock state machine (rev 16 BLOCKER1/2): single Lock at entry / single Unlock at
// exit, handlers run in a paired Unlock/relock window, RESCAN (continue) to catch a 2nd reserved MD, clear
// await under mu before the final unlock, `defer timer.Stop()` only (no deferred unlock). f23 (boss-ordered
// restoration): floor restores the 56e071f b2Floor=500ms / budget=1500ms tool boundary, resolve restores the
// 10de4d8-removed drainForNewFinal(3s) AskQ in-flight-text wait (CompleteCount>baseline) — see stream.go.
func (s *SessionEventStore) WaitForMatchOrDeadlineFloored(sessionID, ptuPromptID string, eligible, boundary func(JobMeta) bool, resolve func() bool, floor, deadline time.Time) string {
	if sessionID == "" {
		return "no_session"
	}
	w := s.getOrCreate(sessionID)
	wait := time.Until(deadline)
	if wait < 0 {
		wait = 0
	}
	// The deadline timer Broadcasts so a parked cond.Wait re-evaluates the time-check and breaks.
	timer := time.AfterFunc(wait, func() {
		w.mu.Lock()
		w.cond.Broadcast()
		w.mu.Unlock()
	})
	defer timer.Stop() // no deferred unlock (rev 16 BLOCKER2.1)
	// The floor timer Broadcasts AT the floor so a wait parked with a boundary already queued wakes to honor
	// the floored early-exit at the floor, not only at the (later) deadline. Skipped when the floor is past.
	if fw := time.Until(floor); fw > 0 {
		floorTimer := time.AfterFunc(fw, func() {
			w.mu.Lock()
			w.cond.Broadcast()
			w.mu.Unlock()
		})
		defer floorTimer.Stop()
	}
	w.mu.Lock()
	// Publish the reservation, then Broadcast to wake an already-blocked eligible MD (rev 16 BLOCKER1) so it
	// re-evaluates admission and may over-cap-admit into the reserved slot.
	w.await = awaitReq{active: true, deadline: deadline, promptID: ptuPromptID}
	w.cond.Broadcast()
	branch := "deadline"
	for {
		got, hitBoundary := extractEligibleBeforeBoundaryLocked(w, eligible, boundary) // holds mu
		if len(got) > 0 {
			w.mu.Unlock()
			for _, j := range got {
				err := j.handler()
				if j.done != nil {
					j.done <- err
					close(j.done)
				}
			}
			// Early-resolve check with mu RELEASED (resolve() may take another lock — no w.mu nesting). For
			// AskQ this is CompleteCount>baseline: a NEW text bubble completed via a just-drained MD handler.
			if resolve != nil && resolve() {
				w.mu.Lock()
				branch = "resolved"
				break
			}
			w.mu.Lock()
			continue // rev 16 BLOCKER2.2: RESCAN — catch a 2nd reserved MD; drain until quiescent
		}
		if !time.Now().Before(deadline) { // absolute deadline (B3 budget ceiling)
			branch = "deadline"
			break
		}
		if hitBoundary && !time.Now().Before(floor) { // floored early-exit (b2Floor); floor==zero => immediate
			branch = "boundary_floored"
			break
		}
		w.cond.Wait() // releases+reacquires mu; woken by admit/removal/floor/deadline Broadcast
	}
	w.await = awaitReq{} // clear reservation UNDER mu
	w.mu.Unlock()        // single explicit unlock; mu is always held here
	return branch
}

// ResolveContentAddressedToolBoundary implements fix C (boss decision 2026-10-09, revised per note2's 8-item
// review) for CC main-session PreToolUse: no fixed floor/budget window. Three states, not two (item 1 of the
// review — the first draft's getText returning a single (text, found) pair conflated "tool_use line not yet
// written" with "written, no text block", causing the notification to fire immediately on BOTH, and separately
// made a written-no-text answer fall through into an unmatchable drainContentMatchLocked("")):
//   - lineWritten=false: the tool_use line is not in the transcript yet. Sleep transcriptPollInterval (item 2 —
//     this is a plain timed retry, NOT w.cond.Wait(), because nothing about this condition is a queue event)
//     and loop — re-call getText(). No timeout, no fallback (boss's ruling).
//   - lineWritten=true, hasText=false: the line IS written and the block immediately before it has no text (or
//     a different message.id, or is itself another tool_use/attachment — TailFindToolUse's own job, see its doc comment in cmd/helpers/transcript.go).
//     Return false immediately (caller sends the tool notification now).
//   - lineWritten=true, hasText=true: text is the preceding text block's content. isRendered(text) true -> this
//     text is ALREADY fully rendered in bs.Streams (see stream.go AssembledText) -> return true immediately
//     (caller sends right away, nothing to wait for). isRendered(text) false -> scan the ENTIRE queue (item 3 —
//     the first draft stopped at "other" boundaries, which can hang forever: in all 22 matched rows from round 4
//     the matched (final) delta's MessageDisplay hook is spawned AFTER its own PreToolUse hook by PID order
//     (an EARLIER, non-final delta for the same message can still fire before PreToolUse — see item 4's
//     split-group example below, PID 3089126 before 3089132 — so this PID-order fact is about the final delta
//     specifically, not every delta), so with parallel/overlapping
//     tool calls a different tool's PreToolUse can be enqueued before this text's MD and the old scan would stop
//     there and never find it — content equality to `text` PLUS the promptID filter already guarantee nothing
//     but the intended group is ever touched, so a boundary stop protects nothing here) via
//     drainContentMatchLocked for the MessageDisplay job group (filtered to promptID, grouped by messageID,
//     SEEDED with whatever of that messageID's deltas are already in bs.Streams — item 4, see below) whose
//     assembled text equals `text`. Found -> run its handlers now, return true. Not found -> publish the
//     existing bounded over-cap admission reservation with deadline=now+noTimeoutHorizon (no real timeout) and
//     block on w.cond.Wait() (this phase DOES have a queue-broadcast event to wake on: a new MessageDisplay job
//     always arrives via enqueueBlocking, which always Broadcasts, session_queue.go:130 and :136) — then loop, re-running
//     getText/isRendered each time since a Stop or further transcript writes can change the answer.
//
// toolUseID does NOT appear in this function's signature — it is only needed by the
// getText closure (to ask TailFindToolUse about ONE specific tool_use_id), which register.go builds and passes
// in; this package never needs to know it.
func (s *SessionEventStore) ResolveContentAddressedToolBoundary(
	sessionID, promptID string,
	getText func() (text string, hasText, lineWritten bool),
	seedGroup func(messageID string) (deltas map[int]string, finalIdx int, hasFinal bool),
	isRendered func(text string) bool,
) bool {
	if sessionID == "" {
		return false
	}
	w := s.getOrCreate(sessionID)
	for {
		text, hasText, lineWritten := getText()
		if !lineWritten {
			time.Sleep(transcriptPollInterval)
			continue
		}
		if !hasText {
			return false
		}
		if isRendered(text) {
			return true
		}
		w.mu.Lock()
		got, matched := drainContentMatchLocked(w, promptID, text, seedGroup)
		if matched {
			w.mu.Unlock()
			for _, j := range got {
				err := j.handler()
				if j.done != nil {
					j.done <- err
					close(j.done)
				}
			}
			return true
		}
		w.await = awaitReq{active: true, deadline: time.Now().Add(noTimeoutHorizon), promptID: promptID}
		w.cond.Broadcast()
		w.cond.Wait() // woken by any enqueue/dequeue/removal on THIS queue — re-check getText/isRendered next loop
		w.await = awaitReq{}
		w.mu.Unlock()
	}
}

// drainContentMatchLocked front-scans the ENTIRE w.jobs (caller holds w.mu; item 3 — no boundary stop of any
// kind, unlike extractEligibleBeforeBoundaryLocked). Every job that is not a MessageDisplay with a matching
// promptID is skipped over untouched (this subsumes the first draft's special "skip this tool's own PostToolUse
// without stopping" rule — there is no special case left: ALL non-matching jobs, including every kind of
// boundary, are just skipped, never inspected further). Matching MessageDisplay jobs are grouped by messageID;
// each group is SEEDED (item 4) via seedGroup(messageID) with whatever deltas for that messageID are already
// merged into bs.Streams (register.go's AppendDelta may have already processed earlier-arriving deltas for this
// SAME messageID before this PreToolUse's job was even dequeued — single-worker FIFO order means an earlier-
// enqueued MessageDisplay job for the same bubble can fully complete before PreToolUse's job is dequeued, while
// a LATER delta for that same messageID — enqueued after PreToolUse's job began running — still sits queued;
// example from note2's review: session 2624141, prompt 6c5f37f4, tool toolu_07cce9f5 — delta hook PID 3089126
// fired BEFORE the PreToolUse hook PID 3089132 and was already drained into bs.Streams by the time this PreToolUse
// ran; delta hook PID 3089135 fired AFTER and was still queued. A scan that only looks at queued jobs can never
// assemble a complete 0..finalIdx run in this case — assembleFromIndex always fails at index 0 — so it would
// never match and the wait would never end). After each queued delta is folded into its group's byIndex map (on
// top of the seed), the group is checked for completeness+equality to `text`; the FIRST group to match is
// removed from w.jobs — only its OWN queued jobs (the seed is not represented by any queued job, so there is
// nothing of the seed's to remove) — preserving the relative order of everything else. If the scan reaches the
// end of w.jobs with no match, matched=false and w.jobs is unchanged (pure lookahead).
func drainContentMatchLocked(w *sessionWorker, promptID, text string, seedGroup func(string) (map[int]string, int, bool)) (got []*sessionEventJob, matched bool) {
	type matchGroup struct {
		byIndex  map[int]string
		finalIdx int
		hasFinal bool
		jobs     []*sessionEventJob
	}
	groups := map[string]*matchGroup{}
	for _, j := range w.jobs {
		if j.event != "MessageDisplay" || j.promptID != promptID {
			continue // every non-matching job, incl. every kind of boundary, is skipped, never touched
		}
		g, ok := groups[j.messageID]
		if !ok {
			seedDeltas, seedFinalIdx, seedHasFinal := seedGroup(j.messageID)
			byIndex := make(map[int]string, len(seedDeltas)+1)
			for k, v := range seedDeltas {
				byIndex[k] = v
			}
			g = &matchGroup{byIndex: byIndex, finalIdx: seedFinalIdx, hasFinal: seedHasFinal}
			groups[j.messageID] = g
		}
		g.byIndex[j.index] = j.delta
		g.jobs = append(g.jobs, j)
		if j.final {
			g.finalIdx = j.index
			g.hasFinal = true
		}
		if !g.hasFinal {
			continue
		}
		assembled, complete := assembleFromIndex(g.byIndex, g.finalIdx)
		if !complete || assembled != text {
			continue
		}
		matchSet := make(map[*sessionEventJob]bool, len(g.jobs))
		for _, gj := range g.jobs {
			matchSet[gj] = true
		}
		kept := make([]*sessionEventJob, 0, len(w.jobs)-len(matchSet))
		for _, wj := range w.jobs {
			if !matchSet[wj] {
				kept = append(kept, wj)
			}
		}
		w.jobs = kept
		w.cond.Broadcast()
		return g.jobs, true
	}
	return nil, false
}

// assembleFromIndex concatenates byIndex[0..finalIdx] in order; incomplete (a gap) -> ("", false).
func assembleFromIndex(byIndex map[int]string, finalIdx int) (string, bool) {
	var b strings.Builder
	for i := 0; i <= finalIdx; i++ {
		d, ok := byIndex[i]
		if !ok {
			return "", false
		}
		b.WriteString(d)
	}
	return b.String(), true
}
