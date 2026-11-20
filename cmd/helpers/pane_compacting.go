package helpers

import (
	"regexp"
	"strings"
)

// piCompactingLine is pi's compaction status line: a braille spinner frame, a space, then "Compacting context..."
// (manual /compact) or "Auto-compacting..." (threshold compaction).
var piCompactingLine = regexp.MustCompile(`^[\x{2800}-\x{28FF}] (Compacting context|Auto-compacting)\.\.\.`)

// codexCompacting reports whether the codex pane shows its compaction status line at the live status position:
// the nearest line above the bottom-most "› " input prompt line that is neither blank nor a "└ " sub-line. Every
// codex history item starts with "• " too, so the line is not matched anywhere else.
func codexCompacting(viewport string) bool {
	lines := strings.Split(viewport, "\n")
	i := len(lines) - 1
	for ; i >= 0 && !strings.HasPrefix(lines[i], "› "); i-- {
	}
	for i--; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" && !strings.HasPrefix(t, "└ ") {
			return strings.HasPrefix(t, "• Compacting context (")
		}
	}
	return false
}

// piCompacting reports whether the pi pane shows its compaction status line: the nearest non-blank line above
// the top rule of the input box, which is the second full-width rule from the bottom.
func piCompacting(viewport string) bool {
	lines := strings.Split(viewport, "\n")
	var rules []int
	for i, ln := range lines {
		if isRuleLine(strings.TrimSpace(ln)) {
			rules = append(rules, i)
		}
	}
	if len(rules) < 2 {
		return false
	}
	for i := rules[len(rules)-2] - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return piCompactingLine.MatchString(t)
		}
	}
	return false
}
