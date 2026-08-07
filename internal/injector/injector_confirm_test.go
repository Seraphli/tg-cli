package injector

import (
	"os/exec"
	"sync"
	"testing"
	"time"
)

// TestGetInjectLock_Identity proves the structural guarantee behind the f29 C transaction: getInjectLock
// returns the SAME per-target mutex on repeat calls and a DIFFERENT one for a different target, so
// InjectTextConfirmSubmit and InjectText/InjectTextAppend all serialize on the one per-target lock.
func TestGetInjectLock_Identity(t *testing.T) {
	a := TmuxTarget{PaneID: "%1"}
	b := TmuxTarget{PaneID: "%2"}
	if getInjectLock(a) != getInjectLock(a) {
		t.Error("same target must return the same *sync.Mutex")
	}
	if getInjectLock(a) == getInjectLock(b) {
		t.Error("different targets must return different mutexes")
	}
}

// TestInjectTextConfirmSubmit_HoldsLockAgainstAppend is the f29 C atomicity regression: while
// InjectTextConfirmSubmit holds the per-target inject lock (its confirm callback blocking), a competing
// InjectTextAppend on the SAME target MUST block until the transaction completes — it can never interleave
// its own C-u/paste between our paste and our Enter. Uses the injector's own tmux primitives so it hits the
// same server; skipped when tmux is unavailable.
func TestInjectTextConfirmSubmit_HoldsLockAgainstAppend(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	const sess = "tgcli-f29c-test"
	globalTmuxCmd("kill-session", "-t", sess).Run()
	if err := CreateSession(sess, ""); err != nil {
		t.Skipf("cannot create tmux session: %v", err)
	}
	defer globalTmuxCmd("kill-session", "-t", sess).Run()
	panes, err := ListPanes(sess)
	if err != nil || len(panes) == 0 {
		t.Skipf("cannot list panes: %v", err)
	}
	target := TmuxTarget{PaneID: panes[0]}

	var mu sync.Mutex
	var order []string
	record := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }

	inConfirm := make(chan struct{})
	releaseConfirm := make(chan struct{})
	appendDone := make(chan struct{})
	var once sync.Once

	go func() {
		confirm := func(pane string) bool {
			once.Do(func() { close(inConfirm) })
			<-releaseConfirm
			record("transaction-submit")
			return true
		}
		InjectTextConfirmSubmit(target, "hello world", confirm, 30*time.Second, 50*time.Millisecond)
	}()

	// Wait until the transaction is inside the confirm callback (paste done, lock held).
	select {
	case <-inConfirm:
	case <-time.After(15 * time.Second):
		t.Fatal("transaction never reached the confirm callback (paste failed?)")
	}

	// Launch a competing append on the same target; it must block on the inject lock until we release.
	go func() {
		InjectTextAppend(target, "APPENDED")
		record("append-ran")
		close(appendDone)
	}()

	// Give the append a chance to (wrongly) interleave while the transaction holds the lock.
	time.Sleep(300 * time.Millisecond)
	select {
	case <-appendDone:
		t.Fatal("InjectTextAppend interleaved while the transaction held the inject lock")
	default:
	}

	close(releaseConfirm) // let the transaction submit (Enter) and unlock
	select {
	case <-appendDone:
	case <-time.After(15 * time.Second):
		t.Fatal("append did not proceed after the transaction released the lock")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(order) < 2 || order[0] != "transaction-submit" || order[1] != "append-ran" {
		t.Errorf("expected [transaction-submit, append-ran], got %v", order)
	}
}
