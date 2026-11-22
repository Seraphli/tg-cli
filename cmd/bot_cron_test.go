package cmd

import (
	"testing"
	"time"

	"github.com/Seraphli/tg-cli/cmd/stores"
)

// TestCheckAndRunCronJobs_OnceJobInjectedOnceWhileFIFOBlocked: the 2026-11-20 incident. A once job (schedule 1m,
// inject, /compact) is due while the session's hook queue is blocked (a Stop job waiting on Telegram). The 30 s
// ticker must not start the job again while its first run still waits: one run, one "inject:safe" in the queue.
func TestCheckAndRunCronJobs_OnceJobInjectedOnceWhileFIFOBlocked(t *testing.T) {
	bs := newHookRig(t, "cc").bs
	const sid = "sess-cron"
	// A nonexistent tmux socket: when the blocked run is released its injection fails and never reaches a real pane.
	bs.SessionState.Add(sid, "%1@/tmp/tg-cli-test-none", "/work", "")
	bs.SessionState.SetName(sid, "sleeper")
	bs.CronJobs = stores.NewCronJobStore(t.TempDir())
	t0 := time.Now()
	bs.CronJobs.Add(&stores.CronJob{ID: "a6fc63c9-job", Name: "sleep-now-sleeper", Mode: "inject", Schedule: "1m", Once: true,
		Prompt: "/compact", AgentName: "sleeper", NoHeader: true, CreatedAt: t0.Add(-time.Minute)})
	release := make(chan struct{})
	bs.SessionEvents.DispatchAsync(sid, "block", func() error { <-release; return nil })
	for k := 0; k < 8; k++ { // ticks every 30 s of job time, up to the 210 s of the incident
		checkAndRunCronJobs(bs, t0.Add(time.Duration(k)*30*time.Second))
		time.Sleep(50 * time.Millisecond) // the run goroutine registers its inject and sets LastRun
	}
	if depth := bs.SessionEvents.QueueDepth(sid); depth != 1 {
		t.Fatalf("%d injections wait in the session queue, want 1", depth)
	}
	close(release)
	waitUntil(t, 3*time.Second, func() bool { return len(bs.CronJobs.All()) == 0 })
}
