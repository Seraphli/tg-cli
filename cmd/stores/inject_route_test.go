package stores

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRouteInjectMode_Decision verifies the PURE routing decision (R2/R5): the triggering hook event +
// tool name select the delivery mode. No tmux/TG involved — this is the unit-testable core.
func TestRouteInjectMode_Decision(t *testing.T) {
	cases := []struct {
		name  string
		event string
		tool  string
		want  InjectMode
	}{
		{"pretooluse non-askq -> queued-command", "PreToolUse", "Bash", InjectModeQueuedCommand},
		{"pretooluse askuserquestion -> askq-custom-reply", "PreToolUse", "AskUserQuestion", InjectModeAskQCustomReply},
		{"stop -> idle", "Stop", "", InjectModeIdle},
		{"5s timeout (empty event) -> queued-command", "", "", InjectModeQueuedCommand},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := RouteInjectMode(c.event, c.tool); got != c.want {
				t.Fatalf("RouteInjectMode(%q,%q)=%d want %d", c.event, c.tool, got, c.want)
			}
		})
	}
}

// TestInjectRoute_ExactlyOnceClaim verifies ArmRoute admits EXACTLY ONE pending routing decision per
// target: the first caller wins, a concurrent/second MD-final (or supplement check) is rejected until
// Release. After Release a fresh window can be armed.
func TestInjectRoute_ExactlyOnceClaim(t *testing.T) {
	ir := NewInjectRouteStore()
	target := "%29@/tmp/tmux-1000/test"

	_, _, won1 := ir.ArmRoute(target, time.Second)
	if !won1 {
		t.Fatal("first ArmRoute should win the claim")
	}
	if !ir.IsArmed(target) {
		t.Fatal("target should be armed after the winning claim")
	}
	// Second claim (a duplicate MD-final or a racing supplement check) must be rejected.
	ch2, tool2, won2 := ir.ArmRoute(target, time.Second)
	if won2 || ch2 != nil || tool2 != nil {
		t.Fatal("second ArmRoute must be rejected while a window is armed")
	}
	// Release frees the claim; a fresh window can be armed.
	ir.Release(target)
	if ir.IsArmed(target) {
		t.Fatal("target should not be armed after Release")
	}
	if _, _, won3 := ir.ArmRoute(target, time.Second); !won3 {
		t.Fatal("ArmRoute after Release should win a fresh claim")
	}
	ir.Release(target)
}

// TestInjectRoute_ConcurrentClaimSingleWinner verifies that under concurrent ArmRoute calls (multiple
// MD-finals + supplement checks racing), EXACTLY ONE wins the claim. -race exercises the mutex.
func TestInjectRoute_ConcurrentClaimSingleWinner(t *testing.T) {
	ir := NewInjectRouteStore()
	target := "%42@/tmp/tmux-1000/test"

	const n = 32
	var wins int32
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if _, _, won := ir.ArmRoute(target, time.Second); won {
				atomic.AddInt32(&wins, 1)
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("expected exactly 1 claim winner, got %d", wins)
	}
	ir.Release(target)
}

// TestInjectRoute_SignalEventRoutesFirstOnly verifies a subsequent hook event delivered via SignalEvent
// reaches the armed window, produces the mode expected by RouteInjectMode, and that only the FIRST
// subsequent hook routes (a second SignalEvent is rejected). Covers PreToolUse(non-AskQ), AskQ, Stop.
func TestInjectRoute_SignalEventRoutesFirstOnly(t *testing.T) {
	cases := []struct {
		name  string
		event string
		tool  string
		want  InjectMode
	}{
		{"pretooluse non-askq", "PreToolUse", "Read", InjectModeQueuedCommand},
		{"pretooluse askq", "PreToolUse", "AskUserQuestion", InjectModeAskQCustomReply},
		{"stop", "Stop", "", InjectModeIdle},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ir := NewInjectRouteStore()
			target := "%7@/tmp/tmux-1000/test"
			eventCh, toolCh, won := ir.ArmRoute(target, time.Second)
			if !won {
				t.Fatal("ArmRoute should win")
			}
			// First subsequent hook routes.
			if !ir.SignalEvent(target, c.event, c.tool) {
				t.Fatal("first SignalEvent should be accepted")
			}
			// A second subsequent hook (another PreToolUse/Stop) must NOT double-handle.
			if ir.SignalEvent(target, "Stop", "") {
				t.Fatal("second SignalEvent must be rejected (only the first hook routes)")
			}
			var gotEvent, gotTool string
			select {
			case gotEvent = <-eventCh:
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for the routed event")
			}
			select {
			case gotTool = <-toolCh:
			default:
			}
			if got := RouteInjectMode(gotEvent, gotTool); got != c.want {
				t.Fatalf("routed mode=%d want %d (event=%q tool=%q)", got, c.want, gotEvent, gotTool)
			}
			ir.Release(target)
		})
	}
}

// TestInjectRoute_SignalEventNoWindow verifies SignalEvent returns false when no window is armed (the
// supplement path then arms + routes on its own event, exercised in routeInjectQueue).
func TestInjectRoute_SignalEventNoWindow(t *testing.T) {
	ir := NewInjectRouteStore()
	if ir.SignalEvent("%99@/tmp/tmux-1000/test", "PreToolUse", "Bash") {
		t.Fatal("SignalEvent with no armed window must return false")
	}
}

// TestInjectRoute_TimeoutMode verifies the 5s-timeout path (no subsequent hook) maps to queued-command:
// the armed window drains no event, so the router uses the empty event -> RouteInjectMode -> queued-command.
func TestInjectRoute_TimeoutMode(t *testing.T) {
	ir := NewInjectRouteStore()
	target := "%13@/tmp/tmux-1000/test"
	eventCh, _, won := ir.ArmRoute(target, 50*time.Millisecond)
	if !won {
		t.Fatal("ArmRoute should win")
	}
	// No SignalEvent — the window's select would time out. Mirror the router's timeout branch.
	var event string
	select {
	case event = <-eventCh:
	case <-time.After(80 * time.Millisecond):
		event = "" // timeout
	}
	if got := RouteInjectMode(event, ""); got != InjectModeQueuedCommand {
		t.Fatalf("timeout should map to queued-command, got mode=%d", got)
	}
	ir.Release(target)
}
