package helpers

import (
	"regexp"
	"strings"
	"time"

	"github.com/Seraphli/tg-cli/internal/injector"
)

const (
	codexOverlayFooter = "Browsing transcript"
	codexPlaceholder   = "Ask Codex to do anything"
)

var (
	sgrRe = regexp.MustCompile(`\x1b\[([0-9;]*)m`)
	// codexStatusRe matches the status line below the composer ("  <model> <effort> · <cwd>"); with an empty composer a
	// hint line ("  ? for shortcuts ...") lies below it.
	codexStatusRe = regexp.MustCompile(`^\s{2}\S.* · [~/]\S*$`)
)

// reversedText is the text of one -e capture line drawn in reverse video (SGR 7), without escape codes.
func reversedText(line string) string {
	on, pos := false, 0
	var b strings.Builder
	for _, m := range sgrRe.FindAllStringSubmatchIndex(line, -1) {
		if on {
			b.WriteString(line[pos:m[0]])
		}
		ps := strings.Split(line[m[2]:m[3]], ";")
		for j := 0; j < len(ps); j++ {
			switch ps[j] {
			case "38", "48":
				// skip the colour operands (38;5;n, 38;2;r;g;b): a number there can equal 7 without being reverse video
				if j+1 < len(ps) {
					switch ps[j+1] {
					case "5":
						j += 2
					case "2":
						j += 4
					}
				}
			case "", "0", "27":
				on = false
			case "7":
				on = true
			}
		}
		pos = m[1]
	}
	if on {
		b.WriteString(line[pos:])
	}
	return b.String()
}

// codexSelection is the selected prompt of the transcript overlay: the reverse-video rows joined by newlines, the
// status line (the last non-empty line) excluded.
func codexSelection(ansi string) string {
	lines := strings.Split(ansi, "\n")
	last := -1
	for i, ln := range lines {
		if strings.TrimSpace(sgrRe.ReplaceAllString(ln, "")) != "" {
			last = i
		}
	}
	var rows []string
	for i, ln := range lines {
		if t := strings.TrimSpace(reversedText(ln)); t != "" && i != last {
			rows = append(rows, t)
		}
	}
	return strings.Join(rows, "\n")
}

// codexOverlayOpen reports the transcript overlay: its footer ("Browsing transcript · ...") is the last non-empty line of the viewport, below the composer row and the status line the overlay redraws. The same words in the conversation above are not the overlay.
func codexOverlayOpen(v string) bool {
	lines := strings.Split(strings.TrimRight(v, " \n"), "\n")
	return strings.HasPrefix(strings.TrimSpace(lines[len(lines)-1]), codexOverlayFooter)
}

// codexComposer is the text of the composer: the last "› " row of the plain viewport and its continuation rows
// (two-space indent) up to the status line; placeholder reports the "Ask Codex to do anything" placeholder. ok is
// false when the composer (status line and a "› " row) is not on screen, and while the transcript overlay is open: the
// overlay draws the same composer row and status line above its own footer, but keys sent there act on the overlay.
func codexComposer(v string) (text string, placeholder, ok bool) {
	if codexOverlayOpen(v) {
		return "", false, false
	}
	lines := strings.Split(v, "\n")
	status := -1
	for i, ln := range lines {
		lines[i] = strings.TrimRight(ln, " ")
		if codexStatusRe.MatchString(lines[i]) {
			status = i
		}
	}
	if status < 0 {
		return "", false, false
	}
	start := -1
	for i, ln := range lines[:status] {
		if strings.HasPrefix(ln, "› ") {
			start = i
		}
	}
	if start < 0 {
		return "", false, false
	}
	out := []string{strings.TrimPrefix(lines[start], "› ")}
	for _, ln := range lines[start+1 : status] {
		out = append(out, strings.TrimPrefix(ln, "  "))
	}
	text = strings.TrimRight(strings.Join(out, "\n"), "\n ")
	return text, text == codexPlaceholder, true
}

// codexKey sends key and waits up to wait for the ANSI capture to differ from prev; ok is false when it never changed.
func codexKey(t injector.TmuxTarget, key, prev string, wait time.Duration) (string, bool) {
	injector.SendKeys(t, key)
	for deadline := time.Now().Add(wait); time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
		if v, err := injector.CaptureViewportANSI(t); err == nil && v != prev {
			return v, true
		}
	}
	return prev, false
}

// codexOlder presses Left until the capture changes, at most 4 presses of 400 ms each: a Left that codex has not drawn yet may only be slow, so 4 presses in a row that change nothing mean the oldest prompt is selected (the overlay has no end marker and no position counter).
func codexOlder(t injector.TmuxTarget, prev string) (string, bool) {
	for i := 0; i < 4; i++ {
		if v, ok := codexKey(t, "Left", prev, 400*time.Millisecond); ok {
			return v, true
		}
	}
	return prev, false
}

// codexRewind drives codex's transcript overlay (Esc Esc opens it on the newest prompt; Left moves toward older
// prompts, no wrap; Enter rewinds to the selected prompt and puts its text back into the composer).
type codexRewind struct{}

func (codexRewind) open(t injector.TmuxTarget) error {
	if err := (codexRewind{}).clearBox(t); err != nil {
		return err
	}
	// two separate calls 300 ms apart: one send-keys call with both Escapes did not open the overlay (E1, rounds/0/captures/e1-cx-13: the footer read "esc again to edit previous message"); the 300 ms is a gap between the keys, not a time limit
	injector.SendKeys(t, "Escape")
	time.Sleep(300 * time.Millisecond)
	injector.SendKeys(t, "Escape")
	if _, ok := pollViewport(t, 10*time.Second, codexOverlayOpen); !ok {
		return ErrRewindNoMenu
	}
	return nil
}

func (codexRewind) read(t injector.TmuxTarget, max int) ([]RewindEntry, bool, error) {
	ansi, _ := injector.CaptureViewportANSI(t)
	label := codexSelection(ansi)
	if label == "" {
		return nil, false, ErrRewindNoMenu
	}
	entries := []RewindEntry{{Label: label}}
	var more bool
	for {
		next, ok := codexOlder(t, ansi)
		if !ok {
			break
		}
		ansi = next
		if len(entries) == max {
			more = true
			break
		}
		if label = codexSelection(ansi); label == "" {
			return nil, false, ErrRewindNoMenu
		}
		entries = append(entries, RewindEntry{Label: label})
	}
	// a Left that codex has not handled yet is handled later and moves the selection one prompt older (codex app_backtrack.rs step_backtrack_and_highlight, as read by note2; not reproduced live, E11 of round 8), so presses that land together can skip prompts: walk back to the newest prompt and compare every step with what was read (with one recorded prompt there is nothing to compare)
	pos := len(entries) - 1
	if more {
		pos++
	}
	for i := pos - 1; i >= 0; i-- {
		next, ok := codexKey(t, "Right", ansi, 10*time.Second)
		if !ok || codexSelection(next) != entries[i].Label {
			return nil, false, ErrRewindListChanged
		}
		ansi = next
	}
	return entries, more, nil
}

func (codexRewind) pick(t injector.TmuxTarget, idx int) (RewindEntry, error) {
	ansi, _ := injector.CaptureViewportANSI(t)
	for i := 0; i < idx; i++ {
		next, ok := codexKey(t, "Left", ansi, 10*time.Second)
		if !ok {
			return RewindEntry{}, ErrRewindListChanged
		}
		ansi = next
	}
	return RewindEntry{Label: codexSelection(ansi)}, nil
}

func (codexRewind) act(t injector.TmuxTarget, _ RewindEntry, action string) (bool, error) {
	if action != "Rewind conversation to here" {
		return false, ErrRewindNoAction
	}
	injector.SendKeys(t, "Enter")
	if _, ok := pollViewport(t, 60*time.Second, func(v string) bool {
		return strings.Contains(v, "Conversation reverted to this point") && !codexOverlayOpen(v)
	}); !ok {
		return false, ErrRewindSettle
	}
	return true, nil
}

func (codexRewind) close(t injector.TmuxTarget) error {
	return escapeUntil(t, codexOverlayOpen, func(v string) bool { return !codexOverlayOpen(v) })
}

func (codexRewind) boxText(t injector.TmuxTarget) (string, error) {
	text, placeholder, ok := codexComposer(viewportNow(t))
	if !ok {
		return "", ErrRewindNoMenu
	}
	if placeholder {
		return "", nil
	}
	return text, nil
}

// clearBox empties the composer: one C-u clears only the cursor line, so each iteration clears the last line and joins
// it to the one above (BSpace); a trailing empty line is removed with Delete.
func (codexRewind) clearBox(t injector.TmuxTarget) error {
	for try := 0; try < 3; try++ {
		for i := 0; i < 30; i++ {
			text, placeholder, ok := codexComposer(viewportNow(t))
			if !ok {
				return ErrRewindNoMenu
			}
			if placeholder {
				return nil
			}
			if strings.TrimSpace(text) == "" {
				injector.SendKeys(t, "Delete")
			} else {
				injector.SendKeys(t, "C-u")
				injector.SendKeys(t, "BSpace")
			}
			time.Sleep(80 * time.Millisecond)
		}
	}
	return ErrRewindBoxNotEmpty
}
