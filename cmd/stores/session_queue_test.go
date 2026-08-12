package stores

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// mdJob builds an eligible MessageDisplay job with the given prompt id and arrival time.
func mdJob(promptID string, arrivedAt time.Time, h func() error) *sessionEventJob {
	return &sessionEventJob{label: "md", event: "MessageDisplay", promptID: promptID, arrivedAt: arrivedAt, handler: h}
}

// waitFor polls cond within a timeout; fails the test if the predicate never holds.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// TestFIFOOrder: jobs dispatched sequentially run in FIFO order on the worker goroutine.
func TestFIFOOrder(t *testing.T) {
	s := NewSessionEventStore()
	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		i := i
		wg.Add(1)
		s.DispatchAsync("sess", "j", func() error {
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			wg.Done()
			return nil
		})
	}
	wg.Wait()
	for i, v := range order {
		if v != i {
			t.Fatalf("FIFO violated at %d: got %d", i, v)
		}
	}
}

// TestDispatchReturnsHandlerError: sync Dispatch returns the handler's error.
func TestDispatchReturnsHandlerError(t *testing.T) {
	s := NewSessionEventStore()
	want := errors.New("boom")
	if got := s.Dispatch("sess", "j", func() error { return want }); !errors.Is(got, want) {
		t.Fatalf("Dispatch must return handler error, got %v", got)
	}
	// sessionID=="" inline fast path also returns the error.
	if got := s.Dispatch("", "j", func() error { return want }); !errors.Is(got, want) {
		t.Fatalf("inline Dispatch must return handler error, got %v", got)
	}
}

// TestTryDispatchFull: TryDispatchAsync fails once the queue is at cap (worker parked so nothing drains).
func TestTryDispatchFull(t *testing.T) {
	s := NewSessionEventStore()
	w := s.getOrCreate("sess")
	// Park the worker on a blocked first job so it never drains, filling the queue deterministically.
	release := make(chan struct{})
	s.DispatchAsync("sess", "block", func() error { <-release; return nil })
	// Wait until the worker has picked up the blocking job (queue drained back toward empty).
	waitFor(t, time.Second, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return len(w.jobs) == 0
	})
	// Fill exactly cap non-eligible jobs (no await -> no over-cap slot).
	for i := 0; i < sessionQueueCap; i++ {
		if !s.TryDispatchAsync("sess", "fill", func() error { return nil }) {
			t.Fatalf("TryDispatchAsync must accept below cap (i=%d)", i)
		}
	}
	if s.TryDispatchAsync("sess", "over", func() error { return nil }) {
		t.Fatal("TryDispatchAsync must fail at cap")
	}
	close(release)
}

// TestDrainAndRunMatchingStopsAtBoundary: front-scan stops at the boundary, extracts only pre-boundary
// eligible jobs, runs them, and delivers the done error. Per contract DrainAndRunMatching runs from a
// running job on the worker goroutine, so we drive it from inside a worker handler. The driver job blocks
// until the test finishes asserting, so the worker never dequeues the remaining queued jobs (their
// handlers must NOT run — they stay queued in order).
func TestDrainAndRunMatchingStopsAtBoundary(t *testing.T) {
	s := NewSessionEventStore()
	w := s.getOrCreate("sess")
	base := time.Now()
	eligible := func(m JobMeta) bool { return m.Event == "MessageDisplay" }
	boundary := func(m JobMeta) bool { return m.Event == "PreToolUse" }
	var mu sync.Mutex
	var ran []string
	mk := func(name string) func() error {
		return func() error { mu.Lock(); ran = append(ran, name); mu.Unlock(); return nil }
	}
	doneErr := errors.New("done-e")
	preDone := make(chan error, 1)
	drained := make(chan int, 1)
	remSnap := make(chan []*sessionEventJob, 1)
	unblock := make(chan struct{})
	s.DispatchAsync("sess", "driver", func() error {
		// Seat the queue: MD(pre1), MD(pre2), PTU(boundary), MD(post — after boundary).
		w.mu.Lock()
		w.jobs = []*sessionEventJob{
			mdJob("", base, mk("pre1")),
			{label: "md2", event: "MessageDisplay", arrivedAt: base, handler: func() error {
				mu.Lock()
				ran = append(ran, "pre2")
				mu.Unlock()
				return doneErr
			}, done: preDone},
			{label: "ptu", event: "PreToolUse", arrivedAt: base, handler: mk("BOUNDARY")},
			{label: "md3", event: "MessageDisplay", arrivedAt: base, handler: mk("post")},
		}
		w.mu.Unlock()
		drained <- s.DrainAndRunMatching("sess", eligible, boundary)
		w.mu.Lock()
		remSnap <- append([]*sessionEventJob(nil), w.jobs...)
		w.mu.Unlock()
		<-unblock // keep the worker parked so it never dequeues the remaining jobs
		return nil
	})
	if n := <-drained; n != 2 {
		t.Fatalf("expected 2 drained pre-boundary MD, got %d", n)
	}
	mu.Lock()
	gotRan := append([]string(nil), ran...)
	mu.Unlock()
	if len(gotRan) != 2 || gotRan[0] != "pre1" || gotRan[1] != "pre2" {
		t.Fatalf("expected [pre1 pre2] ran, got %v", gotRan)
	}
	if got := <-preDone; !errors.Is(got, doneErr) {
		t.Fatalf("done channel must carry handler error, got %v", got)
	}
	rem := <-remSnap
	if len(rem) != 2 || rem[0].label != "ptu" || rem[1].label != "md3" {
		t.Fatalf("boundary + post MD must remain in order, got %v", rem)
	}
	close(unblock)
}

// TestWaitAbsoluteDeadlineTimeout: with nothing to drain and no boundary, the wait returns at the absolute
// deadline (bounded), does not deadlock.
func TestWaitAbsoluteDeadlineTimeout(t *testing.T) {
	s := NewSessionEventStore()
	s.getOrCreate("sess")
	eligible := func(m JobMeta) bool { return m.Event == "MessageDisplay" }
	boundary := func(m JobMeta) bool { return m.Event == "PreToolUse" }
	deadline := time.Now().Add(80 * time.Millisecond)
	start := time.Now()
	s.WaitForMatchOrDeadline("sess", "", eligible, boundary, deadline)
	elapsed := time.Since(start)
	if elapsed < 60*time.Millisecond {
		t.Fatalf("wait returned too early: %v", elapsed)
	}
	if elapsed > 800*time.Millisecond {
		t.Fatalf("wait overran deadline: %v", elapsed)
	}
}

// TestWaitEarlyBroadcastWake: an eligible MD appended AFTER the wait starts is drained PROMPTLY (the enqueue
// Broadcast wakes the parked cond.Wait so the MD is not missed until the deadline timer). The wait itself
// still drains-until-quiescent and returns at the absolute deadline (spec: draining does not short-circuit
// the wait). The observable "early wake" is that mdRan flips well before the deadline. Run inside a driver
// job (worker parked) so the wait — not the idle worker — drains the MD.
func TestWaitEarlyBroadcastWake(t *testing.T) {
	s := NewSessionEventStore()
	w := s.getOrCreate("sess")
	base := time.Now()
	deadline := base.Add(400 * time.Millisecond)
	eligible := func(m JobMeta) bool { return m.Event == "MessageDisplay" && !m.ArrivedAt.After(deadline) }
	boundary := func(m JobMeta) bool { return m.Event == "PreToolUse" || m.ArrivedAt.After(deadline) }
	var mdRan atomic.Bool
	waitReturned := make(chan struct{})
	waitStart := make(chan struct{})
	s.DispatchAsync("sess", "driver", func() error {
		close(waitStart)
		s.WaitForMatchOrDeadline("sess", "", eligible, boundary, deadline)
		close(waitReturned)
		return nil
	})
	<-waitStart
	// Wait until await is published, then append an eligible MD directly (worker is parked inside driver, so
	// appending under mu + Broadcast wakes the wait's cond).
	waitFor(t, time.Second, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.await.active
	})
	appendAt := time.Now()
	w.mu.Lock()
	w.jobs = append(w.jobs, mdJob("", base, func() error { mdRan.Store(true); return nil }))
	w.cond.Broadcast()
	w.mu.Unlock()
	// The Broadcast must wake the parked wait so the MD is drained PROMPTLY — well before the deadline timer.
	waitFor(t, 200*time.Millisecond, func() bool { return mdRan.Load() })
	if drainLatency := time.Since(appendAt); drainLatency > 150*time.Millisecond {
		t.Fatalf("MD drained too slowly (%v) — the enqueue Broadcast did not wake the wait", drainLatency)
	}
	// The wait itself returns at the absolute deadline (draining does not short-circuit it).
	select {
	case <-waitReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("wait never returned")
	}
}

// TestWaitBoundaryInQueueZeroGrace: a boundary already queued behind the PTU makes the wait return
// immediately (no boundaryGrace paid) — rev 15 MAJOR1. Run inside a driver job (worker parked) so the
// seated boundary is not dequeued by the idle worker before the wait scans it.
func TestWaitBoundaryInQueueZeroGrace(t *testing.T) {
	s := NewSessionEventStore()
	w := s.getOrCreate("sess")
	base := time.Now()
	deadline := base.Add(2 * time.Second)
	eligible := func(m JobMeta) bool { return m.Event == "MessageDisplay" }
	boundary := func(m JobMeta) bool { return m.Event == "PreToolUse" }
	elapsedCh := make(chan time.Duration, 1)
	activeCh := make(chan bool, 1)
	unblock := make(chan struct{})
	s.DispatchAsync("sess", "driver", func() error {
		// Seat a boundary behind the (implicit) PTU.
		w.mu.Lock()
		w.jobs = append(w.jobs, &sessionEventJob{label: "ptu2", event: "PreToolUse", arrivedAt: base, handler: func() error { return nil }})
		w.mu.Unlock()
		start := time.Now()
		s.WaitForMatchOrDeadline("sess", "", eligible, boundary, deadline)
		elapsedCh <- time.Since(start)
		w.mu.Lock()
		activeCh <- w.await.active
		w.mu.Unlock()
		<-unblock
		return nil
	})
	if elapsed := <-elapsedCh; elapsed > 200*time.Millisecond {
		t.Fatalf("boundary-in-queue must return immediately (zero grace), took %v", elapsed)
	}
	if <-activeCh {
		t.Fatal("await must be cleared after the wait returns")
	}
	close(unblock)
}

// TestBlockingDispatchWakeup: Dispatch blocked at cap is woken and admitted once the worker drains a slot.
func TestBlockingDispatchWakeup(t *testing.T) {
	s := NewSessionEventStore()
	w := s.getOrCreate("sess")
	release := make(chan struct{})
	s.DispatchAsync("sess", "block", func() error { <-release; return nil })
	waitFor(t, time.Second, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return len(w.jobs) == 0
	})
	// Fill cap.
	for i := 0; i < sessionQueueCap; i++ {
		s.DispatchAsync("sess", "fill", func() error { return nil })
	}
	// This blocking Dispatch must park until a slot frees.
	admitted := make(chan struct{})
	go func() {
		s.DispatchAsync("sess", "late", func() error { return nil })
		close(admitted)
	}()
	select {
	case <-admitted:
		t.Fatal("blocking Dispatch admitted while queue full")
	case <-time.After(50 * time.Millisecond):
	}
	close(release) // worker drains, freeing slots, waking the blocked appender
	select {
	case <-admitted:
	case <-time.After(2 * time.Second):
		t.Fatal("blocking Dispatch never admitted after slot freed")
	}
}

// TestOverCapBoundedToOneEligibleMD: while a wait is active, exactly ONE precise-eligible MD over-cap
// admits (len reaches cap+1); a NON-eligible MD (wrong prompt) does NOT bypass the cap.
func TestOverCapBoundedToOneEligibleMD(t *testing.T) {
	s := NewSessionEventStore()
	w := s.getOrCreate("sess")
	base := time.Now()
	deadline := base.Add(5 * time.Second)
	// Park the worker so the queue can be filled to cap and stay there.
	release := make(chan struct{})
	s.DispatchAsync("sess", "block", func() error { <-release; return nil })
	waitFor(t, time.Second, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return len(w.jobs) == 0
	})
	for i := 0; i < sessionQueueCap; i++ {
		s.DispatchAsync("sess", "fill", func() error { return nil })
	}
	// Publish an await reservation (promptID "P") directly so admission opens the single over-cap slot.
	w.mu.Lock()
	w.await = awaitReq{active: true, deadline: deadline, promptID: "P"}
	w.mu.Unlock()
	// A NON-eligible MD (wrong prompt) must NOT bypass the cap.
	if s.TryDispatchAsyncWithMeta("sess", "bad", "MessageDisplay", "OTHER", base, func() error { return nil }) {
		t.Fatal("non-eligible MD (wrong prompt) must not over-cap admit")
	}
	// A precise-eligible MD (matching prompt, within deadline) takes the ONE over-cap slot -> len cap+1.
	if !s.TryDispatchAsyncWithMeta("sess", "good", "MessageDisplay", "P", base, func() error { return nil }) {
		t.Fatal("precise-eligible MD must over-cap admit")
	}
	w.mu.Lock()
	n := len(w.jobs)
	w.mu.Unlock()
	if n != sessionQueueCap+1 {
		t.Fatalf("over-cap must be bounded to cap+1, got %d", n)
	}
	// A SECOND eligible MD must NOT admit (only one over-cap slot; len already cap+1).
	if s.TryDispatchAsyncWithMeta("sess", "good2", "MessageDisplay", "P", base, func() error { return nil }) {
		t.Fatal("second eligible MD must not admit beyond cap+1")
	}
	close(release)
}

// TestPublishAwaitBroadcastsPreBlockedAppender: an eligible MD blocked at cap on the not-full Cond BEFORE
// the wait publishes await is woken by the publish Broadcast and over-cap admits (rev 16 BLOCKER1).
func TestPublishAwaitBroadcastsPreBlockedAppender(t *testing.T) {
	s := NewSessionEventStore()
	w := s.getOrCreate("sess")
	base := time.Now()
	deadline := base.Add(600 * time.Millisecond)
	eligible := func(m JobMeta) bool { return m.Event == "MessageDisplay" && !m.ArrivedAt.After(deadline) }
	boundary := func(m JobMeta) bool { return m.Event == "PreToolUse" || m.ArrivedAt.After(deadline) }
	// Park the worker and fill to cap.
	release := make(chan struct{})
	s.DispatchAsync("sess", "block", func() error { <-release; return nil })
	waitFor(t, time.Second, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return len(w.jobs) == 0
	})
	for i := 0; i < sessionQueueCap; i++ {
		s.DispatchAsync("sess", "fill", func() error { return nil })
	}
	// Block an eligible MD appender at cap BEFORE await is published (no over-cap slot yet).
	mdRan := make(chan struct{})
	go func() {
		s.DispatchAsyncWithMeta("sess", "md", "MessageDisplay", "P", base, func() error { close(mdRan); return nil })
	}()
	// Confirm it is parked (queue stays at cap, MD not admitted).
	time.Sleep(30 * time.Millisecond)
	w.mu.Lock()
	parked := len(w.jobs) == sessionQueueCap
	w.mu.Unlock()
	if !parked {
		t.Fatal("MD should be blocked at cap before await is published")
	}
	// Now run the wait; publishing await Broadcasts, waking the blocked MD to over-cap admit, then the wait
	// drains it. Release the worker AFTER so drained handlers run.
	waitDone := make(chan struct{})
	go func() {
		s.WaitForMatchOrDeadline("sess", "P", eligible, boundary, deadline)
		close(waitDone)
	}()
	// The wait drains the MD (its handler runs inside the wait's paired window) -> mdRan closes.
	select {
	case <-mdRan:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("blocked eligible MD was not woken/drained by the publish Broadcast")
	}
	close(release)
	<-waitDone
}

// TestTwoReservedMDContend: two eligible MD Dispatch callers contend for the single reserved slot while the
// PTU waits; both are drained (admit-then-drain-then-rescan), queue never exceeds cap+1, no double-unlock
// panic (rev 16 BLOCKER2).
func TestTwoReservedMDContend(t *testing.T) {
	s := NewSessionEventStore()
	w := s.getOrCreate("sess")
	base := time.Now()
	deadline := base.Add(600 * time.Millisecond)
	eligible := func(m JobMeta) bool { return m.Event == "MessageDisplay" && !m.ArrivedAt.After(deadline) }
	boundary := func(m JobMeta) bool { return m.Event == "PreToolUse" || m.ArrivedAt.After(deadline) }
	// Park the worker and fill to cap so both MD must use the (single) reserved slot serially.
	release := make(chan struct{})
	s.DispatchAsync("sess", "block", func() error { <-release; return nil })
	waitFor(t, time.Second, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return len(w.jobs) == 0
	})
	for i := 0; i < sessionQueueCap; i++ {
		s.DispatchAsync("sess", "fill", func() error { return nil })
	}
	// Guard: assert the queue length never exceeds cap+1 throughout the test.
	var maxLen atomic.Int64
	stopMon := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopMon:
				return
			default:
			}
			w.mu.Lock()
			l := int64(len(w.jobs))
			w.mu.Unlock()
			for {
				m := maxLen.Load()
				if l <= m || maxLen.CompareAndSwap(m, l) {
					break
				}
			}
		}
	}()
	var ran atomic.Int64
	// Two eligible MD Dispatch callers contend for the single reserved slot.
	for i := 0; i < 2; i++ {
		go func() {
			s.DispatchWithMeta("sess", "md", "MessageDisplay", "P", base, func() error { ran.Add(1); return nil })
		}()
	}
	// Wait so at least one MD is parked at cap before the wait publishes await.
	time.Sleep(30 * time.Millisecond)
	waitDone := make(chan struct{})
	go func() {
		s.WaitForMatchOrDeadline("sess", "P", eligible, boundary, deadline)
		close(waitDone)
	}()
	// Both MD handlers must run (drained by the wait: admit one -> drain -> rescan -> admit+drain the second).
	waitFor(t, 3*time.Second, func() bool { return ran.Load() == 2 })
	<-waitDone
	close(stopMon)
	close(release)
	if maxLen.Load() > sessionQueueCap+1 {
		t.Fatalf("queue exceeded cap+1: max observed %d", maxLen.Load())
	}
}

// TestExtractStopsAtPostToolUse: with an S6-shaped boundary that INCLUDES PostToolUse (matching the fixed
// register.go), the front-scan STOPS at THIS tool's own PostToolUse — so a post-tool MessageDisplay queued
// behind PostToolUse but before Stop is NOT drained into the pre-tool flush. The extract returns nothing and
// reports hitBoundary. Mirrors the driver-job construction of the other extract tests (worker parked so the
// idle worker never dequeues the seated jobs while we scan them).
func TestExtractStopsAtPostToolUse(t *testing.T) {
	s := NewSessionEventStore()
	w := s.getOrCreate("sess")
	base := time.Now()
	// eligible: any MessageDisplay. boundary: S6-shaped, INCLUDING PostToolUse.
	eligible := func(m JobMeta) bool { return m.Event == "MessageDisplay" }
	boundary := func(m JobMeta) bool {
		return m.Event == "PreToolUse" || m.Event == "PostToolUse" || m.Event == "Stop" ||
			m.Event == "UserPromptSubmit" || m.Event == "SessionEnd"
	}
	noop := func() error { return nil }
	gotCh := make(chan []*sessionEventJob, 1)
	hitCh := make(chan bool, 1)
	unblock := make(chan struct{})
	s.DispatchAsync("sess", "driver", func() error {
		// Seat the queue: PostToolUse(boundary), MD(post — behind the boundary), Stop.
		w.mu.Lock()
		w.jobs = []*sessionEventJob{
			{event: "PostToolUse", promptID: "p1", arrivedAt: base, handler: noop},
			{event: "MessageDisplay", promptID: "p1", arrivedAt: base, handler: noop},
			{event: "Stop", promptID: "p1", arrivedAt: base, handler: noop},
		}
		got, hitBoundary := extractEligibleBeforeBoundaryLocked(w, eligible, boundary)
		w.mu.Unlock()
		gotCh <- got
		hitCh <- hitBoundary
		<-unblock // keep the worker parked so it never dequeues the seated jobs
		return nil
	})
	got := <-gotCh
	hitBoundary := <-hitCh
	if len(got) != 0 {
		t.Fatalf("post-tool MD must NOT be extracted (scan stops at PostToolUse boundary), got %d extracted", len(got))
	}
	if !hitBoundary {
		t.Errorf("expected hitBoundary == true (PostToolUse is a boundary), got false")
	}
	close(unblock)
}
