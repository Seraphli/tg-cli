package helpers

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Seraphli/tg-cli/internal/injector"
)

const (
	piTreeTitle   = "Session Tree"
	piDialogTitle = "Summarize branch?"
	// rule lines of the bottom block while the session tree (rule, title, help, search line, rule, rows, "(k/m)" counter, closing rule) or the summary dialog (rule, blank line, title, options, hint, closing rule) is open; an editor has two
	piTreeRules   = 3
	piDialogRules = 2
)

var (
	piCount  = regexp.MustCompile(`\((\d+)/(\d+)\)`)
	piRow    = regexp.MustCompile(`^\s*›\s+.*?user: (.*?)\s*$`)
	piOption = regexp.MustCompile(`^\s*(→)?\s*(No summary|Summarize with custom prompt|Summarize)\s*$`)
)

// piRuleLines returns the indexes of the rule lines (isRuleLine) of the viewport lines.
func piRuleLines(lines []string) []int {
	var rules []int
	for i, ln := range lines {
		if isRuleLine(strings.TrimSpace(ln)) {
			rules = append(rules, i)
		}
	}
	return rules
}

// piBlock is the menu block of the viewport: from the first non-blank line below the n-th last rule line down to the end, when that line is title; "" otherwise. pi draws its menus in the bottom block between rule lines (piTreeRules, piDialogRules), so the same words in the conversation above are not a menu.
func piBlock(v, title string, n int) string {
	lines := strings.Split(v, "\n")
	rules := piRuleLines(lines)
	if len(rules) < n {
		return ""
	}
	for i := rules[len(rules)-n] + 1; i < len(lines); i++ {
		if t := strings.TrimSpace(lines[i]); t != "" {
			if t != title {
				return ""
			}
			return strings.Join(lines[i:], "\n")
		}
	}
	return ""
}

// piSpinner reports pi's "Summarizing branch..." progress line. pi draws it in the conversation area directly above the editor, so it is the last non-blank line above the editor's top rule line (the second-last rule line); the same words in a message higher up are conversation text.
func piSpinner(v string) bool {
	lines := strings.Split(v, "\n")
	rules := piRuleLines(lines)
	if len(rules) < 2 {
		return false
	}
	for i := rules[len(rules)-2] - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return strings.Contains(t, "Summarizing branch")
		}
	}
	return false
}

// piBoxText is the editor text: the lines between the last two rule lines of the viewport (isRuleLine). ok is false
// without two rule lines and while the session tree or the summary dialog is on screen (piMenuOpen): both are framed
// by rule lines too, so the lines between the last two would be menu rows, not editor text.
func piBoxText(v string) (text string, ok bool) {
	if piMenuOpen(v) {
		return "", false
	}
	lines := strings.Split(v, "\n")
	rules := piRuleLines(lines)
	if len(rules) < 2 {
		return "", false
	}
	return strings.TrimRight(strings.Join(lines[rules[len(rules)-2]+1:rules[len(rules)-1]], "\n"), "\n "), true
}

// piMenuOpen reports the session tree or the summary dialog on screen.
func piMenuOpen(v string) bool {
	return piBlock(v, piTreeTitle, piTreeRules) != "" || piBlock(v, piDialogTitle, piDialogRules) != ""
}

// piTree reads the selector part of the viewport (the block from the "Session Tree" title down, piBlock): the text after "user: " of the
// row under the cursor (›) and the cursor position k of m from the "(k/m)" counter. ok is false without the title, a
// selected user row or a counter. The filtered list shows the oldest message at the top and starts with the cursor on
// the newest one at the bottom (counter k = m), so older messages are reached with Up.
func piTree(v string) (label string, k, m int, ok bool) {
	if v = piBlock(v, piTreeTitle, piTreeRules); v == "" {
		return "", 0, 0, false
	}
	for _, ln := range strings.Split(v, "\n") {
		if g := piRow.FindStringSubmatch(ln); g != nil {
			label = g[1]
			break
		}
	}
	// the counter line is below the rows, so the last match is the counter; a "(1/2)" in a message text sits above it
	cs := piCount.FindAllStringSubmatch(v, -1)
	if label == "" || cs == nil {
		return "", 0, 0, false
	}
	c := cs[len(cs)-1]
	k, _ = strconv.Atoi(c[1])
	m, _ = strconv.Atoi(c[2])
	return label, k, m, true
}

// piClear empties the editor. One C-u clears only the cursor line, so each pass also sends BSpace to join the line above;
// a whitespace-only remainder gets Delete. Each of the 3 tries runs up to 30 passes, 80 ms apart.
func piClear(t injector.TmuxTarget) error {
	for try := 0; try < 3; try++ {
		for i := 0; i < 30; i++ {
			text, ok := piBoxText(viewportNow(t))
			if !ok {
				return ErrRewindNoMenu
			}
			if text == "" {
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

// piEffect is the effect of a pi navigation (No summary or Summarize): the picked message is back in the editor with
// pi's "Navigated to selected point" line on screen. The editor is empty before the action (RewindDrive clears it), so
// an older "Navigated to selected point" line alone proves nothing; a summarize that was aborted or failed leaves the
// editor empty.
func piEffect(v string) bool {
	text, ok := piBoxText(v)
	return ok && text != "" && strings.Contains(v, "Navigated to selected point") && !piSpinner(v) && !piMenuOpen(v)
}

// piRewind drives pi's /tree menu (user filter: the list runs oldest at the top to newest at the bottom, Up moves
// toward older messages and wraps at the ends; the action dialog is navigated with Up/Down and Enter).
type piRewind struct{}

func (piRewind) open(t injector.TmuxTarget) error {
	if err := piClear(t); err != nil {
		return err
	}
	injector.PasteLocked(t, "/tree", true)
	if _, ok := pollViewport(t, 10*time.Second, func(v string) bool { return piBlock(v, piTreeTitle, piTreeRules) != "" }); !ok {
		return ErrRewindNoMenu
	}
	userTag := func(v string) bool { return strings.Contains(piBlock(v, piTreeTitle, piTreeRules), "[user]") }
	// ctrl+u toggles the user filter, so it is sent only while the tag is not on screen
	for i := 0; i < 2 && !userTag(viewportNow(t)); i++ {
		injector.SendKeys(t, "C-u")
		pollViewport(t, 10*time.Second, userTag)
	}
	if !userTag(viewportNow(t)) {
		return ErrRewindNoMenu
	}
	for n := 0; n < 300; n++ {
		v := viewportNow(t)
		_, k, m, ok := piTree(v)
		if !ok {
			return ErrRewindNoMenu
		}
		if k == m {
			return nil
		}
		if _, ok := press(t, v, "Down"); !ok {
			return ErrRewindNoMenu
		}
	}
	return ErrRewindNoMenu
}

func (piRewind) read(t injector.TmuxTarget, max int) ([]RewindEntry, bool, error) {
	var entries []RewindEntry
	v := viewportNow(t)
	for {
		label, k, _, ok := piTree(v)
		if !ok {
			return nil, false, ErrRewindNoMenu
		}
		entries = append(entries, RewindEntry{Label: label})
		if k == 1 {
			return entries, false, nil
		}
		if len(entries) == max {
			return entries, true, nil
		}
		if v, ok = press(t, v, "Up"); !ok {
			return nil, false, ErrRewindNoMenu
		}
	}
}

func (piRewind) pick(t injector.TmuxTarget, idx int) (RewindEntry, error) {
	v := viewportNow(t)
	for i := 0; i < idx; i++ {
		var ok bool
		if v, ok = press(t, v, "Up"); !ok {
			return RewindEntry{}, ErrRewindListChanged
		}
	}
	label, _, _, ok := piTree(v)
	if !ok {
		return RewindEntry{}, ErrRewindListChanged
	}
	return RewindEntry{Label: label}, nil
}

func (piRewind) act(t injector.TmuxTarget, _ RewindEntry, action string) (bool, error) {
	injector.SendKeys(t, "Enter")
	v, ok := pollViewport(t, 10*time.Second, func(v string) bool { return piBlock(v, piDialogTitle, piDialogRules) != "" })
	if !ok {
		return false, ErrRewindNoMenu
	}
	cur, want, i := -1, -1, 0
	for _, ln := range strings.Split(piBlock(v, piDialogTitle, piDialogRules), "\n") {
		m := piOption.FindStringSubmatch(ln)
		if m == nil {
			continue
		}
		if m[1] != "" {
			cur = i
		}
		if m[2] == action {
			want = i
		}
		i++
	}
	if want < 0 {
		return false, ErrRewindNoAction
	}
	if cur < 0 {
		return false, ErrRewindNoMenu
	}
	key, n := "Down", want-cur
	if n < 0 {
		key, n = "Up", -n
	}
	for ; n > 0; n-- {
		if v, ok = press(t, v, key); !ok {
			return false, ErrRewindNoMenu
		}
	}
	injector.SendKeys(t, "Enter")
	if action == "Summarize" {
		v, ok = summarizeWait(t, piSpinner, piEffect)
	} else {
		v, ok = pollViewport(t, 60*time.Second, piEffect)
	}
	switch {
	case ok:
		return true, nil
	case action != "Summarize":
		return false, ErrRewindSettle
	case piSpinner(v):
		return false, ErrRewindStillSummarizing
	}
	return false, ErrRewindNotConfirmed
}

func (piRewind) close(t injector.TmuxTarget) error {
	return escapeUntil(t, piMenuOpen, func(v string) bool { return !piMenuOpen(v) })
}

func (piRewind) boxText(t injector.TmuxTarget) (string, error) {
	text, ok := piBoxText(viewportNow(t))
	if !ok {
		return "", ErrRewindNoMenu
	}
	return text, nil
}

func (piRewind) clearBox(t injector.TmuxTarget) error {
	return piClear(t)
}
