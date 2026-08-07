package stores

import (
	"sync"
	"time"

	"github.com/Seraphli/tg-cli/internal/logger"
)

// sessionQueueCap is the SOFT cap on queued (not-yet-started) jobs per session worker. Appends block
// (Dispatch/DispatchAsync) or fail (TryDispatchAsync) at the cap, EXCEPT one bounded over-cap slot for a
// single precise-eligible MessageDisplay job while the worker awaits a lagging pre-tool MD (rev 15 MAJOR2).
const sessionQueueCap = 128

// sessionEventJob is one queued unit of work for a session worker. label/event/promptID/arrivedAt are the
// drain-lookahead metadata (event classification + absolute arrival deadline eligibility); handler is the
// work; done (when non-nil) receives the handler error for a sync Dispatch caller.
type sessionEventJob struct {
	label     string
	event     string
	promptID  string
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

// DispatchWithMeta is Dispatch carrying the drain-lookahead metadata (event/promptID/arrivedAt).
func (s *SessionEventStore) DispatchWithMeta(sessionID, label, event, promptID string, arrivedAt time.Time, handler func() error) error {
	if sessionID == "" {
		return handler()
	}
	w := s.getOrCreate(sessionID)
	done := make(chan error, 1)
	w.enqueueBlocking(&sessionEventJob{label: label, event: event, promptID: promptID, arrivedAt: arrivedAt, handler: handler, done: done})
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
		m := JobMeta{Event: j.event, PromptID: j.promptID, ArrivedAt: j.arrivedAt}
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
