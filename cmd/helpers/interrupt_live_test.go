package helpers

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/notify"
)

// TestInterruptIfBusyLiveCC runs the real cc in default permission mode (an allow rule for python3 only) through the
// two scenarios of /t and /stop. The bot is nil: no Telegram message exists for the pending entry, so the freeze edit
// stays deferred in the PendingMsgStore and never reaches the bot. TC-A2 also checks that the rewind driver sends no key to
// an open permission dialog: RewindList returns ErrRewindNoMenu, the dialog stays open and dialog-test.txt is not created.
func TestInterruptIfBusyLiveCC(t *testing.T) {
	target := liveStart(t, "cc", 20, "default")
	tmuxTarget := injector.FormatTarget(target)
	hookRunning, pendingWait, pendingMsg := stores.NewHookRunningStateStore(), stores.NewPendingWaitStore(), stores.NewPendingMsgStore()
	interrupt := func(t *testing.T) bool {
		t.Helper()
		sent, err := InterruptIfBusy(nil, pendingWait, pendingMsg, hookRunning, notify.FormatPaneID, target)
		if err != nil {
			t.Fatal(err)
		}
		return sent
	}
	waitBusy := func(t *testing.T, timeout time.Duration, busy bool) {
		t.Helper()
		liveWait(t, target, timeout, func(string) bool {
			state, _ := LiveBusyState(hookRunning, tmuxTarget)
			return (state != stores.BusyIdle) == busy
		})
	}
	// TC-A1: /t on a stopped agent sends nothing, so two quick presses open no rewind menu.
	t.Run("TC-A1", func(t *testing.T) {
		// one finished turn first: without a message two Escapes show nothing
		liveTurn(t, target, "Reply with exactly: alpha", "alpha")
		if err := injector.InjectText(target, `Run this command in the foreground with the Bash tool and wait for it: python3 -c "import time; time.sleep(45)"`); err != nil {
			t.Fatal(err)
		}
		waitBusy(t, 30*time.Second, true)
		// interrupt while the tool runs: an Escape before the tool starts makes cc put the prompt back into the box instead of printing "Interrupted"
		liveWait(t, target, 30*time.Second, func(v string) bool { return strings.Contains(v, "ctrl+b to run in background") })
		if !interrupt(t) {
			t.Fatal("InterruptIfBusy sent nothing to a busy cc")
		}
		liveWait(t, target, 15*time.Second, func(v string) bool { return strings.Contains(v, "Interrupted") })
		waitBusy(t, 15*time.Second, false)
		for i := 1; i <= 2; i++ {
			if interrupt(t) {
				t.Fatalf("call %d on the stopped cc sent an Escape", i)
			}
			time.Sleep(200 * time.Millisecond)
		}
		time.Sleep(1500 * time.Millisecond)
		if v := viewportNow(target); strings.Contains(v, ccMenuFooter) {
			t.Fatalf("the rewind menu is open:\n%s", v)
		}
		if text, ok := ccBoxNow(target); !ok || text != "" {
			t.Fatalf("the input box is %q (box found: %v), want it empty", text, ok)
		}
	})
	// TC-A2: /t on an open permission dialog dismisses it once; the second press sends nothing.
	t.Run("TC-A2", func(t *testing.T) {
		// `echo` runs without a dialog in default mode; `touch` asks
		const dialog = "Do you want to proceed?"
		if err := injector.InjectText(target, "Use the Bash tool to run exactly: touch dialog-test.txt"); err != nil {
			t.Fatal(err)
		}
		liveWait(t, target, 60*time.Second, func(v string) bool { return strings.Contains(v, dialog) })
		// the rewind driver recognises no input box on a dialog screen and sends no key: an Enter would confirm the dialog's default "Yes"
		if _, _, err := RewindList(target, "cc", 10, noRewindCheck); !errors.Is(err, ErrRewindNoMenu) {
			liveFail(t, target, "RewindList with the dialog open returned %v, want ErrRewindNoMenu", err)
		}
		if v := viewportNow(target); !strings.Contains(v, dialog) {
			liveFail(t, target, "the dialog is gone after RewindList")
		}
		if _, err := os.Stat(filepath.Join(ccWorkDir(t, target), "dialog-test.txt")); err == nil {
			liveFail(t, target, "dialog-test.txt exists: a key confirmed the dialog")
		}
		// the PermissionRequest hook registers this entry in a real session; no hook runs here
		pendingWait.Register(&stores.PendingWaitEntry{UUID: "perm1", ToolName: "Bash", TmuxTarget: tmuxTarget})
		if !interrupt(t) {
			t.Fatal("InterruptIfBusy sent nothing with a permission dialog open")
		}
		liveWait(t, target, 5*time.Second, func(v string) bool { return !strings.Contains(v, dialog) })
		if snap, ok := pendingWait.GetSnapshot("perm1"); !ok || !snap.Resolved {
			t.Fatalf("entry perm1 found: %v, resolved: %v, want it resolved", ok, snap.Resolved)
		}
		// the turn ends a moment after the dialog is gone; a press during that moment is a press on a busy agent
		waitBusy(t, 15*time.Second, false)
		if interrupt(t) {
			t.Fatal("the second call sent an Escape")
		}
	})
}
