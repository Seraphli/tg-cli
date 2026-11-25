package helpers

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Seraphli/tg-cli/internal/injector"
)

var (
	ErrRewindNoMenu           = errors.New("could not open the rewind menu")
	ErrRewindBoxNotEmpty      = errors.New("the input box cannot be cleared")
	ErrRewindBoxPasted        = errors.New("the input box holds a paste placeholder, not the message")
	ErrRewindListChanged      = errors.New("the list changed")
	ErrRewindNoAction         = errors.New("the menu does not offer that action")
	ErrRewindSettle           = errors.New("still working after 60 s")
	ErrRewindNotConfirmed     = errors.New("the rewind was not confirmed on screen")
	ErrRewindStillSummarizing = errors.New("still summarizing after 10 min")
)

// RewindEntry is one message a CLI's rewind menu offers. Label is the text as the menu shows it (cc: the first line,
// ending in "…" for a multi-line message; codex: the prompt text; pi: the row text after "user: "); Annotation is
// cc's "No code changes" or "<file> +N" line and empty for codex and pi.
type RewindEntry struct{ Label, Annotation string }

// RewindResult is what a drive leaves: the text the CLI put back into the input box (the box is cleared again, so
// the caller shows it) and the viewport afterwards. InBox is true when the box could not be read in full or could not
// be cleared after the action; the rewind itself has happened.
type RewindResult struct {
	Restored, Pane string
	InBox          bool // the CLI put the message back and the box could not be read in full or cleared: the message is still in the box
}

// rewindDriver is one CLI's menu. Every method runs under the per-target injection mutex (RewindList, RewindDrive).
type rewindDriver interface {
	open(t injector.TmuxTarget) error                                                      // clear the box, open the menu, cursor on the start position
	read(t injector.TmuxTarget, max int) ([]RewindEntry, bool, error)                      // entries newest first from the start position; more = older ones exist
	pick(t injector.TmuxTarget, idx int) (RewindEntry, error)                              // move from the start position to entry idx, return the entry under the cursor
	act(t injector.TmuxTarget, want RewindEntry, action string) (restored bool, err error) // press the action on the picked entry want, wait until its effect is on screen
	close(t injector.TmuxTarget) error                                                     // leave the menu: Escape (900 ms apart) only while the menu is on screen, then wait for the box; no key on a screen that shows neither
	boxText(t injector.TmuxTarget) (string, error)                                         // text in the input box ("" when empty)
	clearBox(t injector.TmuxTarget) error                                                  // empty the box fully
}

func rewindDriverFor(backend string) (rewindDriver, error) {
	switch backend {
	case "cc":
		return ccRewind{}, nil
	case "codex":
		return codexRewind{}, nil
	case "pi":
		return piRewind{}, nil
	}
	return nil, fmt.Errorf("rewind: unsupported backend %q", backend)
}

// RewindActions are the buttons of the action step: exactly what the CLI's own menu offers (cc by the entry's
// annotation, codex one action, pi without "Summarize with custom prompt"); "Never mind" is the bot's Cancel button.
func RewindActions(backend string, e RewindEntry) []string {
	switch backend {
	case "cc":
		if e.Annotation == "No code changes" {
			return []string{"Restore conversation", "Summarize from here", "Summarize up to here"}
		}
		return []string{"Restore code and conversation", "Restore conversation", "Restore code", "Summarize from here", "Summarize up to here"}
	case "codex":
		return []string{"Rewind conversation to here"}
	}
	return []string{"No summary", "Summarize"}
}

// RewindRefusedError is a refused rewind step; Msg is the reply text (pane gone, dialog pending, agent busy).
type RewindRefusedError struct{ Msg string }

func (e *RewindRefusedError) Error() string { return e.Msg }

// RewindList reads up to max messages (newest first) from the CLI's menu and closes the menu again; more reports older
// ones beyond max. The injection mutex is held from the check through the box clear to the verified close. check
// returns a refusal text ("" = go on) and runs first inside the lock: a check outside it leaves a gap in which a
// Telegram message or cron inject makes the agent busy and open() would paste "/rewind" into a busy pane.
func RewindList(target injector.TmuxTarget, backend string, max int, check func() string) (entries []RewindEntry, more bool, err error) {
	d, err := rewindDriverFor(backend)
	if err != nil {
		return nil, false, err
	}
	err = injector.WithInjectLock(target, func() error {
		if msg := check(); msg != "" {
			return &RewindRefusedError{msg}
		}
		if err := d.open(target); err != nil {
			// close a menu that a half-finished open left on screen (pi: the tree without the user filter); close sends
			// Escape only while its predicate still sees a menu or overlay, so it is safe when nothing is open
			d.close(target)
			return err
		}
		entries, more, err = d.read(target, max)
		if cerr := d.close(target); err == nil {
			err = cerr
		}
		return err
	})
	return entries, more, err
}

// RewindDrive rewinds to entry idx (its stored value is want) with the given action. The cursor check (the entry under
// the cursor must equal want) runs before any action key; on any failure the menu is closed again. When the CLI put
// the message back into the box, its text is read, the box is cleared fully and the text is returned; a box that
// cannot be read (cc shows a long message as a "[Pasted text #N]" placeholder) or cleared sets InBox. check is as in
// RewindList: it runs first, inside the lock, before the box clear.
func RewindDrive(target injector.TmuxTarget, backend string, idx int, want RewindEntry, action string, check func() string) (res RewindResult, err error) {
	d, err := rewindDriverFor(backend)
	if err != nil {
		return res, err
	}
	err = injector.WithInjectLock(target, func() error {
		if msg := check(); msg != "" {
			return &RewindRefusedError{msg}
		}
		if err := d.open(target); err != nil {
			d.close(target) // as in RewindList
			return err
		}
		got, err := d.pick(target, idx)
		if err == nil && got != want {
			err = ErrRewindListChanged
		}
		var restored bool
		if err == nil {
			restored, err = d.act(target, want, action)
		}
		if err != nil {
			// a summarize that is still running is left as it is: no Escape (close sends one while cc's Rewind menu is open)
			if !errors.Is(err, ErrRewindStillSummarizing) {
				d.close(target)
			}
			return err
		}
		if restored {
			// The action key was pressed, so the rewind has happened: a box that cannot be read in full or cleared is reported through InBox, never as an error. Text that was not read is never cleared.
			var berr error
			if res.Restored, berr = d.boxText(target); berr != nil || d.clearBox(target) != nil {
				res.InBox = true
			}
		}
		res.Pane, _ = injector.CapturePane(target)
		return nil
	})
	return res, err
}

// pollViewport captures the viewport every 100 ms until ok(viewport) or the timeout; every capture may take up to 10 s;
// it returns the last viewport.
func pollViewport(t injector.TmuxTarget, timeout time.Duration, ok func(string) bool) (string, bool) {
	deadline := time.Now().Add(timeout)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		v, err := injector.CaptureViewport(ctx, t)
		cancel()
		if err == nil && ok(v) {
			return v, true
		}
		if time.Now().After(deadline) {
			return v, false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// viewportNow is one capture.
func viewportNow(t injector.TmuxTarget) string {
	v, _ := pollViewport(t, 0, func(string) bool { return true })
	return v
}

// summarizeWait waits for the effect of a Summarize action (effect) and keeps waiting while the CLI shows that it is summarizing (sign: cc "Summarizing…" inside the Rewind menu block, pi the "Summarizing branch..." progress line): a summarize is a model call that normally takes 2 to 5 minutes on a long conversation (the boss), so the limit is 10 minutes. It returns done true as soon as effect holds. It gives up (done false) after 60 s without the sign, 10 s after the sign went away (a summarize stopped by /t, an error line or a menu closed by hand must not hold the injection lock) or at 10 minutes. It returns the last viewport: with done false, sign(v) is true only for a summarize still running at the limit.
func summarizeWait(t injector.TmuxTarget, sign, effect func(string) bool) (v string, done bool) {
	start, lastSeen := time.Now(), time.Time{} // lastSeen: when the sign was last on screen, zero until it was seen
	v, _ = pollViewport(t, 10*time.Minute, func(s string) bool {
		if done = effect(s); done {
			return true
		}
		if sign(s) {
			lastSeen = time.Now()
		}
		if lastSeen.IsZero() {
			return time.Since(start) > 60*time.Second
		}
		return time.Since(lastSeen) > 10*time.Second
	})
	return v, done
}

// press sends keys and waits (up to 10 s) until the viewport differs from prev; ok is false when it never changed.
func press(t injector.TmuxTarget, prev string, keys ...string) (string, bool) {
	injector.SendKeys(t, keys...)
	return pollViewport(t, 10*time.Second, func(v string) bool { return v != prev })
}

// escapeUntil sends Escape while the menu is on screen (menuOpen), at most 4 times, 900 ms apart: two Escapes within
// 0.7 s on an idle empty cc box would open the rewind menu again. Once the menu is gone it waits up to 10 s for done
// (the box is back). A screen that shows neither the menu nor the expected box gets no key: an Escape there would
// decline a permission dialog.
func escapeUntil(t injector.TmuxTarget, menuOpen, done func(string) bool) error {
	for i := 0; i < 4; i++ {
		if !menuOpen(viewportNow(t)) {
			if _, ok := pollViewport(t, 10*time.Second, done); ok {
				return nil
			}
			return ErrRewindNoMenu
		}
		injector.SendKeys(t, "Escape")
		time.Sleep(900 * time.Millisecond)
	}
	if _, ok := pollViewport(t, 10*time.Second, func(v string) bool { return !menuOpen(v) && done(v) }); ok {
		return nil
	}
	return ErrRewindNoMenu
}
