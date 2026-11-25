package helpers

import (
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/Seraphli/tg-cli/internal/injector"
)

const (
	ccMenuFooter = "Enter to continue · Esc to cancel"
	ccActionLead = "Confirm you want to restore to the point before you sent this message:"
	ccListLead   = "Restore the code and/or conversation to the point before"
	// ccFailedLine is cc's line in the Rewind block after a failed summarize.
	ccFailedLine = "Failed to summarize"
	ccBorder     = "▔▔▔▔▔▔▔▔▔▔"
	ccBorderRule = "──────────"
)

var (
	// ccOptionLine matches an action-menu option row; the last visible row of a scrolled menu starts with ↓ instead of a space.
	ccOptionLine = regexp.MustCompile(`^\s*(?:[❯↑↓]\s+)?(\d+)\. (.+?)\s*$`)
	ccSGR        = regexp.MustCompile(`\x1b\[[0-9;]*m`)
)

// ccPasteRe matches cc's placeholder for a long pasted message (also "[Pasted text #3 +29 lines]").
var ccPasteRe = regexp.MustCompile(`\[Pasted text #\d+`)

type ccRow struct {
	Entry             RewindEntry
	Selected, Current bool
}

type ccWindow struct {
	Rows         []ccRow
	Above, Below bool
}

// parseCCWindow reads the list level of the Rewind menu block (ccMenuBlock): the lines between the lead line and the footer. A blank line
// ends a block; a block is "label / annotation" or "(current)"; the cursor row starts with ❯. An "↑ N more" / "↓ N more"
// marker is a line of its own: cc prints the ↑ marker directly above the first row without a blank line.
func parseCCWindow(v string) ccWindow {
	lines := strings.Split(ccMenuBlock(v), "\n")
	h, f := -1, -1
	for i, ln := range lines {
		if h < 0 && strings.Contains(ln, ccListLead) {
			h = i
		}
		if strings.Contains(ln, ccMenuFooter) {
			f = i
		}
	}
	var w ccWindow
	if h < 0 || f <= h {
		return w
	}
	var block []string
	flush := func() {
		if len(block) == 0 {
			return
		}
		sel := false
		for i, b := range block {
			b = strings.TrimSpace(b)
			if strings.HasPrefix(b, "❯") {
				sel = true
				b = strings.TrimSpace(strings.TrimPrefix(b, "❯"))
			}
			block[i] = b
		}
		switch {
		case block[0] == "(current)":
			w.Rows = append(w.Rows, ccRow{Current: true, Selected: sel})
		case len(block) >= 2:
			w.Rows = append(w.Rows, ccRow{Entry: RewindEntry{Label: block[0], Annotation: block[1]}, Selected: sel})
		}
		block = nil
	}
	for _, ln := range lines[h+1 : f] {
		t := strings.TrimSpace(ln)
		switch {
		case t == "":
			flush()
		case strings.HasPrefix(t, "↑ ") && strings.HasSuffix(t, " more"):
			flush()
			w.Above = true
		case strings.HasPrefix(t, "↓ ") && strings.HasSuffix(t, " more"):
			flush()
			w.Below = true
		default:
			block = append(block, ln)
		}
	}
	flush()
	return w
}

// selected returns the row under the cursor and whether it is the first (oldest) entry of the whole list: the
// first row of the window with no "↑ N more" marker.
func (w ccWindow) selected() (row ccRow, atTop, ok bool) {
	for i, r := range w.Rows {
		if r.Selected {
			return r, i == 0 && !w.Above, true
		}
	}
	return ccRow{}, false, false
}

// ccBoxText is the text in cc's input box: the first enclosed line without its ❯, continuation lines without their
// two-space indent. ok is false without a box. v is the ANSI capture: a grey prompt suggestion or placeholder (the
// first enclosed line's text after ❯ starts with SGR 2, dim) reads as "" because the plain capture prints it exactly
// like typed text. A plain capture works for the box check (ok) only.
func ccBoxText(v string) (string, bool) {
	raw := strings.Split(v, "\n")
	lines := strings.Split(ccSGR.ReplaceAllString(v, ""), "\n")
	anchor, bottom := ccInputBox(lines)
	if anchor < 0 {
		return "", false
	}
	if _, after, _ := strings.Cut(raw[anchor+1], "❯"); strings.HasPrefix(strings.TrimLeftFunc(after, unicode.IsSpace), "\x1b[2m") {
		return "", true
	}
	var out []string
	for i, ln := range lines[anchor+1 : bottom] {
		ln = strings.TrimRight(ln, " \u00a0")
		if i == 0 {
			ln = strings.TrimLeftFunc(strings.TrimPrefix(strings.TrimLeft(ln, " "), "❯"), unicode.IsSpace)
		} else {
			ln = strings.TrimPrefix(ln, "  ")
		}
		out = append(out, ln)
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n "), true
}

// ccBoxNow reads the box text from the ANSI capture of the pane.
func ccBoxNow(t injector.TmuxTarget) (string, bool) {
	v, err := injector.CaptureViewportANSI(t)
	if err != nil {
		return "", false
	}
	return ccBoxText(v)
}

// ccMenuBlock is the part of the viewport the Rewind menu draws: from its top border (a line that starts with a run of ▔ or, in some panes, of ─; an effort badge or a usage notice may follow the run on the same row) down to the end of the viewport, taken from the last border whose next line is the title "Rewind". It is "" when no such block is on screen: the menu words in a prompt or a reply above it are conversation text, not a menu.
func ccMenuBlock(v string) string {
	lines := strings.Split(v, "\n")
	for i := len(lines) - 2; i >= 0; i-- {
		t := strings.TrimSpace(lines[i])
		if (strings.HasPrefix(t, ccBorder) || strings.HasPrefix(t, ccBorderRule)) && strings.TrimSpace(lines[i+1]) == "Rewind" {
			return strings.Join(lines[i:], "\n")
		}
	}
	return ""
}

// ccMenuOpen is true while the Rewind block is on screen, including the error screen of a failed summarize: that block stays until an Escape, so close must send one.
func ccMenuOpen(v string) bool {
	b := ccMenuBlock(v)
	return strings.Contains(b, ccMenuFooter) || strings.Contains(b, ccActionLead) || ccSummarizeFailed(v)
}

// ccSummarizing reports the "Summarizing…" line of a Summarize action inside the Rewind menu block; the same word in the conversation above is not the menu's line.
func ccSummarizing(v string) bool { return strings.Contains(ccMenuBlock(v), "Summarizing…") }

// ccSummarizeFailed reports the error screen of a failed summarize: the "Failed to summarize" line inside the Rewind menu block; the same words in the conversation above are not the block's line, as with ccSummarizing.
func ccSummarizeFailed(v string) bool { return strings.Contains(ccMenuBlock(v), ccFailedLine) }

// ccEffectFor is the screen condition that ends the wait after the digit of action on the entry want: the effect of the action is on screen, not only the end of the menu. The restore actions are done when the picked message is back in the input box (its text, or cc's "[Pasted text #N]" placeholder for a long one): the box was cleared before the menu opened, so a non-empty box that starts with the entry's label is the message coming back. want.Label is the first line as the menu shows it, ending in "…" when the line was cut or the message has more lines, so the label without that "…" is the start of the box text. A Summarize action keeps the Rewind menu on screen with its own "Summarizing…" line while the model call runs: it is done when that line has been seen inside the menu block after the digit and the menu is then gone, cc's input box is recognised again (`ccBoxText` ok: the bottom-most region between two rule lines of at least 40 ─ whose first enclosed line starts with ❯) and "Summarized conversation" is on screen (an earlier summary on screen already holds that text, so the line alone proves nothing). A summarize whose block showed "Failed to summarize" is never done, even when a hand Escape then closes the block and an earlier summary is on screen. Restore code puts nothing back and leaves no mark: it is done when the menu is gone and the box is back.
func ccEffectFor(want RewindEntry, action string) func(v string) bool {
	switch action {
	case "Restore code":
		return func(v string) bool { _, box := ccBoxText(v); return !ccMenuOpen(v) && box }
	case "Summarize from here", "Summarize up to here":
		seen, failed := false, false
		return func(v string) bool {
			seen = seen || ccSummarizing(v)
			failed = failed || ccSummarizeFailed(v)
			_, box := ccBoxText(v)
			return seen && !failed && !ccMenuOpen(v) && box && strings.Contains(v, "Summarized conversation")
		}
	}
	head := strings.TrimRight(strings.TrimSuffix(want.Label, "…"), " ")
	return func(v string) bool {
		text, box := ccBoxText(v)
		return !ccMenuOpen(v) && box && text != "" && (strings.HasPrefix(text, head) || ccPasteRe.MatchString(text))
	}
}

// ccRewind drives cc's /rewind menu (list level: Up moves toward older messages; action level: a digit acts at once).
type ccRewind struct{}

func (ccRewind) open(t injector.TmuxTarget) error {
	for i := 0; i < 3; i++ {
		if strings.Contains(ccMenuBlock(viewportNow(t)), ccMenuFooter) {
			if i == 0 {
				// a menu that is already on screen when open starts is one the user left open: its cursor can stand on an older entry and read lists from the cursor on, not from the newest message; no key is sent, RewindList and RewindDrive close the menu and the user runs /rewind again. From the second pass on the menu is the one this function's own Escape pair opened
				return ErrRewindNoMenu
			}
			return nil
		}
		text, ok := ccBoxNow(t)
		if !ok {
			// no input box and no menu: a permission dialog or an unknown screen; no key goes there (Enter would confirm the dialog's default "Yes")
			return ErrRewindNoMenu
		}
		if text != "" {
			// typed text is cleared whole by one Escape pair; a grey suggestion or an empty box opens the menu instead
			injector.SendKeys(t, "Escape", "Escape")
			time.Sleep(900 * time.Millisecond)
			continue
		}
		injector.PasteLocked(t, "/rewind", true)
		if _, ok := pollViewport(t, 10*time.Second, func(v string) bool { return strings.Contains(ccMenuBlock(v), ccMenuFooter) }); ok {
			return nil
		}
	}
	if text, _ := ccBoxNow(t); text != "" {
		return ErrRewindBoxNotEmpty
	}
	return ErrRewindNoMenu
}

func (ccRewind) read(t injector.TmuxTarget, max int) ([]RewindEntry, bool, error) {
	var entries []RewindEntry
	prev := viewportNow(t)
	for {
		v, ok := press(t, prev, "Up")
		if !ok {
			return nil, false, ErrRewindNoMenu
		}
		row, atTop, found := parseCCWindow(v).selected()
		if !found || row.Current || row.Entry.Label == "" {
			return nil, false, ErrRewindNoMenu
		}
		entries = append(entries, row.Entry)
		if atTop {
			return entries, false, nil
		}
		if len(entries) == max {
			return entries, true, nil
		}
		prev = v
	}
}

func (ccRewind) pick(t injector.TmuxTarget, idx int) (RewindEntry, error) {
	prev := viewportNow(t)
	for i := 0; i < idx+1; i++ {
		v, ok := press(t, prev, "Up")
		if !ok {
			return RewindEntry{}, ErrRewindListChanged
		}
		prev = v
	}
	row, _, found := parseCCWindow(prev).selected()
	if !found || row.Current {
		return RewindEntry{}, ErrRewindListChanged
	}
	return row.Entry, nil
}

func (ccRewind) act(t injector.TmuxTarget, want RewindEntry, action string) (bool, error) {
	injector.SendKeys(t, "Enter")
	v, ok := pollViewport(t, 10*time.Second, func(v string) bool { return strings.Contains(ccMenuBlock(v), ccActionLead) })
	if !ok {
		return false, ErrRewindNoMenu
	}
	digit := ""
	for _, ln := range strings.Split(ccMenuBlock(v), "\n") {
		if m := ccOptionLine.FindStringSubmatch(ln); m != nil && m[2] == action {
			digit = m[1]
			break
		}
	}
	if digit == "" {
		return false, ErrRewindNoAction
	}
	injector.SendKeys(t, digit)
	summarize := strings.HasPrefix(action, "Summarize")
	if summarize {
		v, ok = summarizeWait(t, ccSummarizing, ccEffectFor(want, action))
	} else {
		v, ok = pollViewport(t, 60*time.Second, ccEffectFor(want, action))
	}
	switch {
	case ok:
		return action != "Restore code", nil
	case action == "Restore code":
		return false, ErrRewindSettle
	case summarize && ccSummarizing(v):
		return false, ErrRewindStillSummarizing
	}
	return false, ErrRewindNotConfirmed
}

func (ccRewind) close(t injector.TmuxTarget) error {
	return escapeUntil(t, ccMenuOpen, func(v string) bool { _, ok := ccBoxText(v); return ok })
}

func (ccRewind) boxText(t injector.TmuxTarget) (string, error) {
	text, ok := ccBoxNow(t)
	if !ok {
		return "", ErrRewindNoMenu
	}
	if ccPasteRe.MatchString(text) {
		// cc keeps a long message as a paste: the box holds "[Pasted text #N]", not the text; nothing is read, so RewindDrive sets InBox and leaves the box alone
		return "", ErrRewindBoxPasted
	}
	return text, nil
}

func (ccRewind) clearBox(t injector.TmuxTarget) error {
	for i := 0; i < 3; i++ {
		if text, _ := ccBoxNow(t); text == "" {
			return nil
		}
		injector.SendKeys(t, "Escape", "Escape")
		v, _ := pollViewport(t, 10*time.Second, func(v string) bool {
			text, _ := ccBoxNow(t)
			return text == "" || ccMenuOpen(v)
		})
		if ccMenuOpen(v) {
			// the box was empty or showed a suggestion, so the pair opened the menu
			return ccRewind{}.close(t)
		}
	}
	return ErrRewindBoxNotEmpty
}
