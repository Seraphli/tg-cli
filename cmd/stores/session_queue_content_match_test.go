package stores

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Every test below drives the REAL production ResolveContentAddressedToolBoundary from inside a worker
// handler, never a test-only variant (session_queue_test.go:94's/:188's established driver-job pattern): a
// `s.DispatchAsync("sess", "driver", ...)` seeds w.jobs under w.mu, calls the real method, and reports its
// bool return via a channel. This pins the one real worker goroutine inside the driver job for the test's
// duration, so the production w.run() loop cannot concurrently dequeue/run any seeded job out from under the
// test's own assertions. Closures passed into the method (getText/seedGroup/isRendered) execute on that same
// worker goroutine, so they report back via atomics/channels rather than calling t.Fatal directly (t.Fatal
// must run on the test's own goroutine).

// TestResolveContentAddressedToolBoundary_DrainsBehindOwnPostToolUse (item 3): a PostToolUse job sitting
// AHEAD of the matching MessageDisplay group in the queue is never inspected (not a boundary stop — it is
// simply not a MessageDisplay) and is left untouched in its original position after the group is drained.
func TestResolveContentAddressedToolBoundary_DrainsBehindOwnPostToolUse(t *testing.T) {
	s := NewSessionEventStore()
	w := s.getOrCreate("sess")
	base := time.Now()
	const text = "hello world"
	var mu sync.Mutex
	var ran []string
	mk := func(name string) func() error {
		return func() error { mu.Lock(); ran = append(ran, name); mu.Unlock(); return nil }
	}
	postTool := &sessionEventJob{label: "ptu-post", event: "PostToolUse", promptID: "P", arrivedAt: base, handler: mk("post")}
	md0 := &sessionEventJob{label: "md0", event: "MessageDisplay", promptID: "P", messageID: "m1", index: 0, delta: "hello ", arrivedAt: base, handler: mk("md0")}
	md1 := &sessionEventJob{label: "md1", event: "MessageDisplay", promptID: "P", messageID: "m1", index: 1, final: true, delta: "world", arrivedAt: base, handler: mk("md1")}

	getText := func() (string, bool, bool) { return text, true, true }
	isRendered := func(string) bool { return false }
	seedGroup := func(string) (map[int]string, int, bool) { return nil, -1, false }

	resultCh := make(chan bool, 1)
	unblock := make(chan struct{})
	s.DispatchAsync("sess", "driver", func() error {
		w.mu.Lock()
		w.jobs = []*sessionEventJob{postTool, md0, md1}
		w.mu.Unlock()
		resultCh <- s.ResolveContentAddressedToolBoundary("sess", "P", getText, seedGroup, isRendered)
		<-unblock // keep the worker parked so it never dequeues the remaining job while we assert
		return nil
	})
	if got := <-resultCh; !got {
		t.Fatal("expected true (the MD group matched and ran)")
	}
	mu.Lock()
	gotRan := append([]string(nil), ran...)
	mu.Unlock()
	if len(gotRan) != 2 || gotRan[0] != "md0" || gotRan[1] != "md1" {
		t.Fatalf("expected [md0 md1] ran, got %v", gotRan)
	}
	w.mu.Lock()
	rem := append([]*sessionEventJob(nil), w.jobs...)
	w.mu.Unlock()
	if len(rem) != 1 || rem[0] != postTool {
		t.Fatalf("PostToolUse job must remain untouched in its original position, got %v", rem)
	}
	close(unblock)
}

// TestResolveContentAddressedToolBoundary_WaitsForLateTranscriptWriteNoQueueActivity (item 2): with ZERO
// queue mutations of any kind, getText reporting lineWritten=false for the first K calls must not make the
// method return — it only returns false once getText reports lineWritten=true on the K+1th call. This proves
// the loop advances via its OWN timer (transcriptPollInterval), not via a cond.Wait() nothing here ever wakes.
func TestResolveContentAddressedToolBoundary_WaitsForLateTranscriptWriteNoQueueActivity(t *testing.T) {
	s := NewSessionEventStore()
	w := s.getOrCreate("sess")
	const k = 3
	var calls atomic.Int64
	var otherCalls atomic.Int64    // seedGroup/isRendered must never be reached on this path
	var queueActivity atomic.Int64 // getText calls that saw a queued job or an active await reservation
	// Each getText call snapshots the queue state (getText runs before ResolveContentAddressedToolBoundary takes
	// w.mu), which turns the "zero queue mutations while waiting" claim into a direct assertion.
	getText := func() (string, bool, bool) {
		n := calls.Add(1)
		w.mu.Lock()
		if len(w.jobs) != 0 || w.await.active {
			queueActivity.Add(1)
		}
		w.mu.Unlock()
		if n <= k {
			return "", false, false // not written yet
		}
		return "", false, true // written, no text
	}
	isRendered := func(string) bool { otherCalls.Add(1); return false }
	seedGroup := func(string) (map[int]string, int, bool) { otherCalls.Add(1); return nil, -1, false }

	resultCh := make(chan bool, 1)
	s.DispatchAsync("sess", "driver", func() error {
		resultCh <- s.ResolveContentAddressedToolBoundary("sess", "P", getText, seedGroup, isRendered)
		return nil
	})
	if got := <-resultCh; got {
		t.Fatal("expected false (written, no text)")
	}
	if n := calls.Load(); n != k+1 {
		t.Fatalf("expected exactly %d getText calls (the loop must advance via its own timer), got %d", k+1, n)
	}
	if n := otherCalls.Load(); n != 0 {
		t.Fatalf("isRendered/seedGroup must never be called while hasText=false, got %d calls", n)
	}
	if n := queueActivity.Load(); n != 0 {
		t.Fatalf("a queued job or await reservation was visible in %d getText calls, want none (zero enqueues/dequeues while waiting for the transcript write)", n)
	}
}

// TestResolveContentAddressedToolBoundary_FoundPastAnotherToolsBoundary (item 3): the matching MD group
// sitting BEHIND a DIFFERENT tool's PreToolUse job is still found — the scan never stops at any boundary. The
// other tool's PreToolUse job is left untouched in its original position.
func TestResolveContentAddressedToolBoundary_FoundPastAnotherToolsBoundary(t *testing.T) {
	s := NewSessionEventStore()
	w := s.getOrCreate("sess")
	base := time.Now()
	const text = "past the boundary"
	var mu sync.Mutex
	var ran []string
	mk := func(name string) func() error {
		return func() error { mu.Lock(); ran = append(ran, name); mu.Unlock(); return nil }
	}
	otherPTU := &sessionEventJob{label: "other-ptu", event: "PreToolUse", promptID: "OTHER", arrivedAt: base, handler: mk("other-ptu")}
	md0 := &sessionEventJob{label: "md0", event: "MessageDisplay", promptID: "P", messageID: "m1", index: 0, delta: "past the ", arrivedAt: base, handler: mk("md0")}
	md1 := &sessionEventJob{label: "md1", event: "MessageDisplay", promptID: "P", messageID: "m1", index: 1, final: true, delta: "boundary", arrivedAt: base, handler: mk("md1")}

	getText := func() (string, bool, bool) { return text, true, true }
	isRendered := func(string) bool { return false }
	seedGroup := func(string) (map[int]string, int, bool) { return nil, -1, false }

	resultCh := make(chan bool, 1)
	unblock := make(chan struct{})
	s.DispatchAsync("sess", "driver", func() error {
		w.mu.Lock()
		w.jobs = []*sessionEventJob{otherPTU, md0, md1}
		w.mu.Unlock()
		resultCh <- s.ResolveContentAddressedToolBoundary("sess", "P", getText, seedGroup, isRendered)
		<-unblock
		return nil
	})
	if got := <-resultCh; !got {
		t.Fatal("expected true (MD group found past another tool's boundary)")
	}
	mu.Lock()
	gotRan := append([]string(nil), ran...)
	mu.Unlock()
	if len(gotRan) != 2 || gotRan[0] != "md0" || gotRan[1] != "md1" {
		t.Fatalf("expected [md0 md1] ran, got %v", gotRan)
	}
	w.mu.Lock()
	rem := append([]*sessionEventJob(nil), w.jobs...)
	w.mu.Unlock()
	if len(rem) != 1 || rem[0] != otherPTU {
		t.Fatalf("other tool's PreToolUse job must remain untouched in its original position, got %v", rem)
	}
	close(unblock)
}

// TestResolveContentAddressedToolBoundary_SplitGroupAcrossStreamsAndQueue (item 4): seedGroup supplies index
// 0 (already merged into bs.Streams by an earlier-drained job); only the index-1 final delta is still
// queued. assembleFromIndex can only complete 0..finalIdx by combining the seed with the queued delta.
func TestResolveContentAddressedToolBoundary_SplitGroupAcrossStreamsAndQueue(t *testing.T) {
	s := NewSessionEventStore()
	w := s.getOrCreate("sess")
	base := time.Now()
	const text = "first half second half"
	var mu sync.Mutex
	var ran []string
	mk := func(name string) func() error {
		return func() error { mu.Lock(); ran = append(ran, name); mu.Unlock(); return nil }
	}
	md1 := &sessionEventJob{label: "md1", event: "MessageDisplay", promptID: "P", messageID: "m1", index: 1, final: true, delta: "second half", arrivedAt: base, handler: mk("md1")}

	seedGroup := func(messageID string) (map[int]string, int, bool) {
		if messageID == "m1" {
			return map[int]string{0: "first half "}, -1, false
		}
		return nil, -1, false
	}
	getText := func() (string, bool, bool) { return text, true, true }
	isRendered := func(string) bool { return false }

	resultCh := make(chan bool, 1)
	unblock := make(chan struct{})
	s.DispatchAsync("sess", "driver", func() error {
		w.mu.Lock()
		w.jobs = []*sessionEventJob{md1}
		w.mu.Unlock()
		resultCh <- s.ResolveContentAddressedToolBoundary("sess", "P", getText, seedGroup, isRendered)
		<-unblock
		return nil
	})
	select {
	case got := <-resultCh:
		if !got {
			t.Fatal("expected true (split group across bs.Streams and the queue matched)")
		}
	case <-time.After(2 * time.Second):
		close(unblock)
		t.Fatal("ResolveContentAddressedToolBoundary never returned — seeding not applied, assembleFromIndex stuck at index 0")
	}
	mu.Lock()
	gotRan := append([]string(nil), ran...)
	mu.Unlock()
	if len(gotRan) != 1 || gotRan[0] != "md1" {
		t.Fatalf("expected [md1] ran, got %v", gotRan)
	}
	close(unblock)
}

// TestResolveContentAddressedToolBoundary_NoTextOrAlreadyRendered (item 1): three states, not two.
func TestResolveContentAddressedToolBoundary_NoTextOrAlreadyRendered(t *testing.T) {
	t.Run("not_written_then_written_no_text", func(t *testing.T) {
		// lineWritten alternates false,false,false,true(hasText=false) across 4 calls — a two-state
		// implementation would have returned false already on call 1.
		s := NewSessionEventStore()
		s.getOrCreate("sess")
		var calls atomic.Int64
		var other atomic.Int64
		getText := func() (string, bool, bool) {
			n := calls.Add(1)
			if n < 4 {
				return "", false, false
			}
			return "", false, true
		}
		isRendered := func(string) bool { other.Add(1); return false }
		seedGroup := func(string) (map[int]string, int, bool) { other.Add(1); return nil, -1, false }
		resultCh := make(chan bool, 1)
		s.DispatchAsync("sess", "driver", func() error {
			resultCh <- s.ResolveContentAddressedToolBoundary("sess", "P", getText, seedGroup, isRendered)
			return nil
		})
		if got := <-resultCh; got {
			t.Fatal("expected false")
		}
		if n := calls.Load(); n != 4 {
			t.Fatalf("expected exactly 4 getText calls, got %d", n)
		}
		if n := other.Load(); n != 0 {
			t.Fatalf("isRendered/seedGroup must not be called, got %d calls", n)
		}
	})
	t.Run("written_no_text_immediate", func(t *testing.T) {
		// getText returns (_, false, true) on the FIRST call -> return false immediately, zero calls into
		// seedGroup/drainContentMatchLocked.
		s := NewSessionEventStore()
		s.getOrCreate("sess")
		var calls atomic.Int64
		var other atomic.Int64
		getText := func() (string, bool, bool) { calls.Add(1); return "", false, true }
		isRendered := func(string) bool { other.Add(1); return false }
		seedGroup := func(string) (map[int]string, int, bool) { other.Add(1); return nil, -1, false }
		resultCh := make(chan bool, 1)
		s.DispatchAsync("sess", "driver", func() error {
			resultCh <- s.ResolveContentAddressedToolBoundary("sess", "P", getText, seedGroup, isRendered)
			return nil
		})
		if got := <-resultCh; got {
			t.Fatal("expected false (written, no text)")
		}
		if n := calls.Load(); n != 1 {
			t.Fatalf("expected exactly 1 getText call, got %d", n)
		}
		if n := other.Load(); n != 0 {
			t.Fatalf("isRendered/seedGroup must not be called on the no-text path, got %d", n)
		}
	})
	t.Run("already_rendered_immediate", func(t *testing.T) {
		// getText returns (T, true, true) and isRendered(T) is true -> return true immediately, zero calls
		// into seedGroup/drainContentMatchLocked.
		s := NewSessionEventStore()
		s.getOrCreate("sess")
		const text = "already rendered text"
		var textMismatch atomic.Bool
		var other atomic.Int64
		getText := func() (string, bool, bool) { return text, true, true }
		isRendered := func(got string) bool {
			if got != text {
				textMismatch.Store(true)
			}
			return true
		}
		seedGroup := func(string) (map[int]string, int, bool) { other.Add(1); return nil, -1, false }
		resultCh := make(chan bool, 1)
		s.DispatchAsync("sess", "driver", func() error {
			resultCh <- s.ResolveContentAddressedToolBoundary("sess", "P", getText, seedGroup, isRendered)
			return nil
		})
		if got := <-resultCh; !got {
			t.Fatal("expected true (already rendered)")
		}
		if textMismatch.Load() {
			t.Fatal("isRendered called with unexpected text")
		}
		if n := other.Load(); n != 0 {
			t.Fatalf("seedGroup/drainContentMatchLocked must not be reached when isRendered is true, got %d calls", n)
		}
	})
}

// TestResolveContentAddressedToolBoundary_PostToolTextNeverPulledForward: an UNRELATED MessageDisplay group
// (same promptID, different content) queued behind this tool's own PostToolUse is NEVER pulled forward —
// only content equality to T (plus the promptID filter) resolves the wait. The method parks on w.cond.Wait()
// until the MATCHING group is appended directly to the queue.
func TestResolveContentAddressedToolBoundary_PostToolTextNeverPulledForward(t *testing.T) {
	s := NewSessionEventStore()
	w := s.getOrCreate("sess")
	base := time.Now()
	const text = "the real pre-tool text"
	var unrelatedRan atomic.Int64
	var matchedRan atomic.Int64

	postTool := &sessionEventJob{label: "ptu-post", event: "PostToolUse", promptID: "P", arrivedAt: base, handler: func() error { return nil }}
	unrelated := &sessionEventJob{label: "unrelated-md", event: "MessageDisplay", promptID: "P", messageID: "other-msg", index: 0, final: true,
		delta: "something else entirely", arrivedAt: base, handler: func() error { unrelatedRan.Add(1); return nil }}

	getText := func() (string, bool, bool) { return text, true, true }
	isRendered := func(string) bool { return false }
	seedGroup := func(string) (map[int]string, int, bool) { return nil, -1, false }

	resultCh := make(chan bool, 1)
	unblock := make(chan struct{})
	s.DispatchAsync("sess", "driver", func() error {
		w.mu.Lock()
		w.jobs = []*sessionEventJob{postTool, unrelated}
		w.mu.Unlock()
		resultCh <- s.ResolveContentAddressedToolBoundary("sess", "P", getText, seedGroup, isRendered)
		<-unblock // keep the worker parked so it never dequeues the remaining jobs while we assert
		return nil
	})
	// Confirm the method has actually parked (no match found among the seeded jobs) before proceeding.
	waitFor(t, time.Second, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.await.active
	})
	w.mu.Lock()
	stillThere := len(w.jobs) == 2 && w.jobs[0] == postTool && w.jobs[1] == unrelated
	w.mu.Unlock()
	if !stillThere {
		t.Fatal("the unrelated post-tool MD group must remain queued, untouched, while parked")
	}
	if unrelatedRan.Load() != 0 {
		t.Fatal("the unrelated group's handler must not have run while parked")
	}
	// Append the matching T-content MessageDisplay group directly, then Broadcast to wake the parked Wait
	// (session_queue_test.go:203-206's idiom).
	matching := &sessionEventJob{label: "matching-md", event: "MessageDisplay", promptID: "P", messageID: "match-msg", index: 0, final: true,
		delta: text, arrivedAt: base, handler: func() error { matchedRan.Add(1); return nil }}
	w.mu.Lock()
	w.jobs = append(w.jobs, matching)
	w.cond.Broadcast()
	w.mu.Unlock()

	select {
	case got := <-resultCh:
		if !got {
			t.Fatal("expected true (the matching group was found after being woken)")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ResolveContentAddressedToolBoundary never returned after the matching group was appended")
	}
	if n := matchedRan.Load(); n != 1 {
		t.Fatalf("expected the matching group's handler to run exactly once, got %d", n)
	}
	w.mu.Lock()
	rem := append([]*sessionEventJob(nil), w.jobs...)
	w.mu.Unlock()
	if len(rem) != 2 || rem[0] != postTool || rem[1] != unrelated {
		t.Fatalf("the unrelated group must remain in w.jobs afterward in its original position, got %v", rem)
	}
	if unrelatedRan.Load() != 0 {
		t.Fatal("the unrelated group's handler must still not have run")
	}
	close(unblock)
}
