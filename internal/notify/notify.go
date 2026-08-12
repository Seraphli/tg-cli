package notify

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/Seraphli/tg-cli/internal/markdown"
)

type NotificationData struct {
	Event             string
	Project           string
	CWD               string
	Body              string
	TmuxTarget        string
	ToolName          string
	AgentName         string
	Backend           string // "cc" or "codex"
	CLICommand        string
	Page              int // 0 = no pagination
	TotalPages        int
	ContextUsedPct    int // -1 means no data
	ContextWindowSize int
	ContextUsedTokens int
	Finalized         bool
	// Interrupted (Event=="Message", Item 7): this streamed bubble's pi run was truncated by a retryable
	// provider error that pi is auto-retrying on the same turn — render "🔄 Interrupted — retrying…". Takes
	// precedence over Finalized.
	Interrupted bool
	// Cron fields (Event=="Cron"): the cron job identity for the notification header.
	CronJobID    string
	CronName     string
	CronNoHeader bool
	// SessionSend fields (Event=="SessionSend"): the sender identity for the notification header.
	SendFrom     string
	SendNoHeader bool
	// DeliveryStatus (Event=="SessionSend"): "" | "unconfirmed" | "submit_failed". Annotates the
	// header when the CLI inject reached the pane but delivery/submit was not confirmed.
	DeliveryStatus string
}

// DeliveryStatusTag returns the short header suffix appended to a SessionSend notification when the
// CLI inject reached the pane but delivery was not confirmed. Empty for a normal ("") status.
func DeliveryStatusTag(status string) string {
	switch status {
	case "unconfirmed":
		return " ⚠️ delivery unconfirmed"
	case "submit_failed":
		return " ⚠️ submit failed (likely not executed)"
	default:
		return ""
	}
}

// DeliveryStatusWarning returns the full operator-facing warning text for a soft session-send
// delivery status (printed to the CLI stderr). Empty for a normal ("") status.
func DeliveryStatusWarning(status string) string {
	switch status {
	case "unconfirmed":
		return "delivery unconfirmed: keys sent, confirmation did not arrive - do NOT re-send"
	case "submit_failed":
		return "text pasted but submit FAILED - likely NOT executed; check the pane and submit manually - do NOT re-send"
	default:
		return ""
	}
}

type PermissionData struct {
	Project        string
	CWD            string
	TmuxTarget     string
	ToolName       string
	ToolInput      map[string]interface{}
	SuggestionDesc string
	AgentName      string
	CLICommand     string
}

type QuestionOption struct {
	Label       string
	Description string
}

type QuestionEntry struct {
	Header      string
	Question    string
	Options     []QuestionOption
	MultiSelect bool
}

type QuestionData struct {
	Project           string
	CWD               string
	TmuxTarget        string
	Header            string
	Question          string
	Options           []QuestionOption
	Questions         []QuestionEntry
	AgentName         string
	CLICommand        string
	ContextUsedPct    int
	ContextUsedTokens int
	ContextWindowSize int
}

// CompressPath shortens a filesystem path by abbreviating intermediate components to their first character.
func CompressPath(path string) string {
	home, _ := os.UserHomeDir()
	if home != "" && strings.HasPrefix(path, home) {
		path = "~" + path[len(home):]
	}
	sep := string(os.PathSeparator)
	parts := strings.Split(path, sep)
	if len(parts) <= 2 {
		return path
	}
	for i := 1; i < len(parts)-1; i++ {
		if len(parts[i]) > 0 {
			parts[i] = string([]rune(parts[i])[0])
		}
	}
	return strings.Join(parts, sep)
}

// TailPath returns the last n path segments. Short paths (≤n segments) are returned as-is.
func TailPath(path string, n int) string {
	home, _ := os.UserHomeDir()
	if home != "" && strings.HasPrefix(path, home) {
		path = "~" + path[len(home):]
	}
	sep := string(os.PathSeparator)
	parts := strings.Split(path, sep)
	var segments []string
	for _, p := range parts {
		if p != "" {
			segments = append(segments, p)
		}
	}
	if len(segments) <= n {
		return path
	}
	return strings.Join(segments[len(segments)-n:], sep)
}

// normalizeShellNewlines flattens a multi-line command into one line, quote-aware: a newline outside
// quotes is a top-level command separator (" ; "); a newline inside single/double quotes is argument
// content and becomes a single space (the quoted content is opaque and is truncated by the reducer).
// Quote tracking mirrors tokenizeCommand (single/double quotes; backslash escape inside double quotes).
func normalizeShellNewlines(cmd string) string {
	var b strings.Builder
	inSingle, inDouble := false, false
	runes := []rune(cmd)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r == '\'' && !inDouble:
			inSingle = !inSingle
			b.WriteRune(r)
		case r == '\\' && inDouble && i+1 < len(runes):
			b.WriteRune(r)
			i++
			b.WriteRune(runes[i])
		case r == '"' && !inSingle:
			inDouble = !inDouble
			b.WriteRune(r)
		case r == '\n' && (inSingle || inDouble):
			b.WriteRune(' ')
		case r == '\n':
			b.WriteString(" ; ")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// tokenizeCommand splits a shell command string into tokens, respecting single/double quotes.
// Recognizes &&, ||, ;, | as token boundaries even without surrounding spaces.
func tokenizeCommand(cmd string) []string {
	var tokens []string
	var current strings.Builder
	inSingle, inDouble := false, false
	runes := []rune(cmd)
	flush := func() {
		if current.Len() > 0 {
			tokens = append(tokens, current.String())
			current.Reset()
		}
	}
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r == '\'' && !inDouble:
			inSingle = !inSingle
			current.WriteRune(r)
		case r == '\\' && inDouble && i+1 < len(runes):
			current.WriteRune(r)
			i++
			current.WriteRune(runes[i])
		case r == '"' && !inSingle:
			inDouble = !inDouble
			current.WriteRune(r)
		case inSingle || inDouble:
			current.WriteRune(r)
		case r == ' ':
			flush()
		case r == '&' && i+1 < len(runes) && runes[i+1] == '&':
			flush()
			tokens = append(tokens, "&&")
			i++
		case r == '|' && i+1 < len(runes) && runes[i+1] == '|':
			flush()
			tokens = append(tokens, "||")
			i++
		case r == '|':
			flush()
			tokens = append(tokens, "|")
		case r == ';':
			flush()
			tokens = append(tokens, ";")
		default:
			current.WriteRune(r)
		}
	}
	flush()
	return tokens
}

// splitSubCommands splits a command line by shell operators (&&, ||, ;, |),
// using quote-aware tokenization. Returns alternating command/operator strings
// where odd indices are operators.
func splitSubCommands(cmd string) []string {
	tokens := tokenizeCommand(cmd)
	var parts []string
	var current []string
	for _, t := range tokens {
		if t == "&&" || t == "||" || t == ";" || t == "|" {
			if len(current) > 0 {
				parts = append(parts, strings.Join(current, " "))
				current = nil
			}
			parts = append(parts, t)
		} else {
			current = append(current, t)
		}
	}
	if len(current) > 0 {
		parts = append(parts, strings.Join(current, " "))
	}
	return parts
}

var flagRe = regexp.MustCompile(`^-[a-zA-Z]|^--[a-zA-Z]`)

// knownFlagArgs maps command names to flags that consume the next token as their value.
var knownFlagArgs = map[string]map[string]bool{
	"grep":   {"-A": true, "-B": true, "-C": true, "-m": true, "-f": true, "-e": true},
	"rg":     {"-A": true, "-B": true, "-C": true, "-m": true, "-g": true, "-t": true},
	"ssh":    {"-i": true, "-o": true, "-p": true, "-F": true, "-l": true, "-J": true, "-W": true},
	"scp":    {"-i": true, "-P": true, "-F": true},
	"curl":   {"-X": true, "-H": true, "-d": true, "-o": true, "-u": true, "--max-time": true},
	"find":   {"-name": true, "-iname": true, "-type": true, "-maxdepth": true, "-mindepth": true, "-path": true},
	"git":    {"-C": true, "-c": true},
	"sort":   {"-k": true, "-t": true},
	"awk":    {"-F": true, "-v": true},
	"sed":    {"-i": true, "-e": true},
	"xargs":  {"-I": true, "-n": true, "-P": true},
	"docker": {"-p": true, "-v": true, "-e": true, "-w": true, "--name": true, "--network": true},
	"tar":    {"-f": true, "-C": true},
}

// stripFlagsAware removes flag tokens and their value arguments from a token list.
// Returns (result, omitted) where omitted is true if any flags were removed.
func stripFlagsAware(cmdName string, tokens []string) ([]string, bool) {
	known := knownFlagArgs[cmdName]
	var result []string
	skip := false
	omitted := false
	for i, t := range tokens {
		if skip {
			skip = false
			continue
		}
		if strings.HasPrefix(t, "--") && strings.Contains(t, "=") {
			omitted = true
			continue
		}
		if flagRe.MatchString(t) {
			omitted = true
			if known != nil && known[t] && i+1 < len(tokens) {
				skip = true
			}
			continue
		}
		result = append(result, t)
	}
	return result, omitted
}

var pathRe = regexp.MustCompile(`(?:/home/\S+|~/\S+|/[a-z]+(?:/\S+){3,})`)

// compressPathsInText finds absolute paths in text and replaces them with tail-3.
// When a path is shortened, prepends "…/" to indicate omission.
func compressPathsInText(text string) string {
	return pathRe.ReplaceAllStringFunc(text, func(match string) string {
		trail := ""
		for len(match) > 0 {
			last := match[len(match)-1]
			if last == '"' || last == '\'' || last == ')' || last == ';' || last == ',' {
				trail = string(last) + trail
				match = match[:len(match)-1]
			} else {
				break
			}
		}
		compressed := TailPath(match, 3)
		if compressed != match {
			compressed = "…/" + compressed
		}
		return compressed + trail
	})
}

var redirectionRe = regexp.MustCompile(`^[0-9]*>`)

// isRedirection returns true if token looks like a shell redirection (2>/dev/null, >&2, >file, etc.)
func isRedirection(token string) bool {
	return redirectionRe.MatchString(token)
}

var standaloneRedirRe = regexp.MustCompile(`^[0-9]*>{1,2}$|^[0-9]*>&$`)

// filterRedirections removes redirection tokens from a slice.
// Returns (result, removedText) where removedText is the joined original redirection tokens that were dropped.
// Handles both attached (2>/dev/null) and separated (2> /dev/null) forms.
func filterRedirections(tokens []string) ([]string, string) {
	var result []string
	var removed []string
	skip := false
	for _, tok := range tokens {
		if skip {
			skip = false
			removed = append(removed, tok)
			continue
		}
		if isRedirection(tok) {
			removed = append(removed, tok)
			if standaloneRedirRe.MatchString(tok) {
				// Standalone redirection (e.g. "2>"): the next token is its target; skip and record it.
				skip = true
			}
			continue
		}
		result = append(result, tok)
	}
	return result, strings.Join(removed, " ")
}

// stripLeadingEnvAssignments removes leading VAR=value patterns from a sub-command string.
// Handles simple values (FOO=bar), quoted ("val"), single-quoted ('val'),
// command substitution $(cmd), and brace expansion ${var}.
// Operates on the raw string before tokenization to avoid splitting issues.
func stripLeadingEnvAssignments(s string) string {
	pos := 0
	for pos < len(s) {
		// Skip whitespace
		for pos < len(s) && s[pos] == ' ' {
			pos++
		}
		if pos >= len(s) {
			break
		}
		// Check for identifier followed by =
		start := pos
		if !(s[pos] >= 'A' && s[pos] <= 'Z' || s[pos] >= 'a' && s[pos] <= 'z' || s[pos] == '_') {
			break
		}
		for pos < len(s) && (s[pos] >= 'A' && s[pos] <= 'Z' || s[pos] >= 'a' && s[pos] <= 'z' || s[pos] >= '0' && s[pos] <= '9' || s[pos] == '_') {
			pos++
		}
		if pos >= len(s) || s[pos] != '=' {
			pos = start
			break
		}
		pos++ // skip '='
		// Skip the value
		if pos < len(s) && s[pos] == '"' {
			pos++
			for pos < len(s) {
				if s[pos] == '\\' && pos+1 < len(s) {
					pos += 2
				} else if s[pos] == '"' {
					pos++
					break
				} else {
					pos++
				}
			}
		} else if pos < len(s) && s[pos] == '\'' {
			pos++
			for pos < len(s) && s[pos] != '\'' {
				pos++
			}
			if pos < len(s) {
				pos++
			}
		} else if pos+1 < len(s) && s[pos] == '$' && s[pos+1] == '(' {
			pos += 2
			depth := 1
			for pos < len(s) && depth > 0 {
				if s[pos] == '(' {
					depth++
				} else if s[pos] == ')' {
					depth--
				}
				pos++
			}
		} else if pos+1 < len(s) && s[pos] == '$' && s[pos+1] == '{' {
			pos += 2
			depth := 1
			for pos < len(s) && depth > 0 {
				if s[pos] == '{' {
					depth++
				} else if s[pos] == '}' {
					depth--
				}
				pos++
			}
		} else {
			for pos < len(s) && s[pos] != ' ' {
				pos++
			}
		}
	}
	result := strings.TrimLeft(s[pos:], " ")
	return result
}

// cleanSubCommandParts replaces empty sub-command entries with "…" and preserves operators,
// maintaining the alternating cmd-op-cmd invariant.
func cleanSubCommandParts(parts []string) []string {
	var result []string
	for i, part := range parts {
		if i%2 == 0 {
			if strings.TrimSpace(part) == "" {
				// Empty sub-command: replace with … marker
				result = append(result, "…")
			} else {
				result = append(result, part)
			}
		} else {
			result = append(result, part)
		}
	}
	// Remove trailing operator if present (after … replacement)
	for len(result) > 0 && (result[len(result)-1] == "&&" || result[len(result)-1] == "||" || result[len(result)-1] == ";" || result[len(result)-1] == "|") {
		result = result[:len(result)-1]
	}
	// Remove leading operator if present
	for len(result) > 0 && (result[0] == "&&" || result[0] == "||" || result[0] == ";" || result[0] == "|") {
		result = result[1:]
	}
	// Collapse consecutive … markers separated by operators into single …
	var collapsed []string
	for i, part := range result {
		if i%2 == 0 && part == "…" && i > 0 && i < len(result)-1 {
			// Check if prev and next are both operators (i.e. "… op …" → skip middle)
			// Actually keep them: the operators show structure
			collapsed = append(collapsed, part)
		} else {
			collapsed = append(collapsed, part)
		}
	}
	// Now remove "… op …" sequences: replace with single "…"
	var final []string
	i := 0
	for i < len(collapsed) {
		if i+2 < len(collapsed) && collapsed[i] == "…" && collapsed[i+2] == "…" {
			// Merge: keep the operator between them? No — merge into single …
			final = append(final, "…")
			i += 3
			// Continue merging if next is also … op …
			for i+1 < len(collapsed) && (collapsed[i] == "&&" || collapsed[i] == "||" || collapsed[i] == ";" || collapsed[i] == "|") {
				if i+1 < len(collapsed) && collapsed[i+1] == "…" {
					i += 2
				} else {
					break
				}
			}
		} else {
			final = append(final, collapsed[i])
			i++
		}
	}
	return final
}

func cmdBasename(s string) string {
	if idx := strings.LastIndex(s, "/"); idx >= 0 {
		return s[idx+1:]
	}
	return s
}

// cmdDisplayName returns the basename of a command path for display.
// When the path was not already marked as shortened (i.e. doesn't start with "…/"),
// prepends "…/" to indicate the path was shortened from a longer form.
// If the path has no "/" or was already shortened, returns basename as-is.
func cmdDisplayName(rawBase string) string {
	base := cmdBasename(rawBase)
	if base == rawBase {
		// No path separator — already a bare name
		return base
	}
	if strings.HasPrefix(rawBase, "…/") {
		// Already marked as shortened by compressPathsInText — just return basename
		return base
	}
	// Path was shortened here; add …/ to indicate omission
	return "…/" + base
}

// truncateWithEllipsis truncates s to at most maxBytes bytes including the 3-byte "…" suffix.
func truncateWithEllipsis(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	limit := maxBytes - 3
	if limit <= 0 {
		return s[:maxBytes]
	}
	pos := 0
	for pos < len(s) {
		_, size := utf8.DecodeRuneInString(s[pos:])
		if pos+size > limit {
			break
		}
		pos += size
	}
	return s[:pos] + "…"
}

// truncateMiddle truncates s to at most maxRunes runes (including the 1-rune "…") by removing the middle.
// Suffix-biased: gives 2/3 of the budget to the end so tail keywords are preserved.
func truncateMiddle(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	if maxRunes <= 0 {
		return ""
	}
	if maxRunes == 1 {
		return "…"
	}
	available := maxRunes - 1 // reserve 1 rune for "…"
	frontBudget := available / 3
	if frontBudget < 1 {
		frontBudget = 1
	}
	backBudget := available - frontBudget
	frontPos, frontRunes := 0, 0
	for frontPos < len(s) && frontRunes < frontBudget {
		_, size := utf8.DecodeRuneInString(s[frontPos:])
		frontPos += size
		frontRunes++
	}
	backPos, backRunes := len(s), 0
	for backPos > 0 && backRunes < backBudget {
		_, size := utf8.DecodeLastRuneInString(s[:backPos])
		backPos -= size
		backRunes++
	}
	return s[:frontPos] + "…" + s[backPos:]
}

// stripInlineComment removes a trailing # comment from a line, respecting quotes.
// Only treats # as a comment when at position 0 or preceded by whitespace (foo#bar is NOT a comment).
// Returns (result, omitted) where omitted is true if a comment was stripped.
func stripInlineComment(line string) (string, bool) {
	inSingle, inDouble := false, false
	runes := []rune(line)
	for i, r := range runes {
		switch {
		case r == '\'' && !inDouble:
			inSingle = !inSingle
		case r == '"' && !inSingle:
			inDouble = !inDouble
		case r == '#' && !inSingle && !inDouble && (i == 0 || runes[i-1] == ' ' || runes[i-1] == '\t'):
			stripped := strings.TrimSpace(line[:len(string(runes[:i]))])
			return stripped, stripped != line
		}
	}
	return line, false
}

// stripShellComments removes shell comments from a multi-line command.
// Whole-line comments (lines starting with #) are discarded entirely.
// Inline comments (cmd # comment) are stripped, preserving the command part.
// Quote-aware: # inside quotes is not treated as a comment.
// Returns (result, omitted) where omitted is true if any comments were removed.
func stripShellComments(cmd string) (string, bool) {
	lines := strings.Split(cmd, "\n")
	var result []string
	omitted := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			omitted = true
			continue
		}
		cleaned, inlineOmitted := stripInlineComment(trimmed)
		if inlineOmitted {
			omitted = true
		}
		if cleaned != "" {
			result = append(result, cleaned)
		}
	}
	return strings.Join(result, "\n"), omitted
}

const compactMaxLen = 45

// compactLen returns the rune count of s, used for all budget length checks.
func compactLen(s string) int { return utf8.RuneCountInString(s) }

// tokenSeg represents a classified segment of a sub-command's argument list.
// isFlag is true for flag tokens (and their value when a known flag consumes one).
type tokenSeg struct {
	text   string
	isFlag bool
}

// classifyArgSegments classifies the arg tokens (tokens[1:]) of a sub-command into ordered
// segments, each marked as a flag span or a plain arg. Reuses the same detection as
// stripFlagsAware: --x=y inline, flagRe short/long flags, knownFlagArgs value-skipping.
// Each returned segment spans one logical unit (flag-only or flag+value pair, or a plain arg).
func classifyArgSegments(cmdName string, tokens []string) []tokenSeg {
	known := knownFlagArgs[cmdName]
	var segs []tokenSeg
	i := 0
	for i < len(tokens) {
		t := tokens[i]
		if strings.HasPrefix(t, "--") && strings.Contains(t, "=") {
			segs = append(segs, tokenSeg{text: t, isFlag: true})
			i++
			continue
		}
		if flagRe.MatchString(t) {
			if known != nil && known[t] && i+1 < len(tokens) {
				segs = append(segs, tokenSeg{text: t + " " + tokens[i+1], isFlag: true})
				i += 2
			} else {
				segs = append(segs, tokenSeg{text: t, isFlag: true})
				i++
			}
			continue
		}
		segs = append(segs, tokenSeg{text: t, isFlag: false})
		i++
	}
	return segs
}

// subCmdModel is the internal model used by reduceToBudget for one sub-command part.
type subCmdModel struct {
	leadMarker  bool   // true when a leading "… " prefix exists (env/comment omission)
	name        string // command name (tokens[0])
	segs        []tokenSeg
	trailMarker bool // true when a trailing " …" suffix exists (redirection removal)
	removed     []bool
}

// renderModels joins all subCmdModel entries with their operators into a single string.
// Applies cmdDisplayName to command names to shorten path-like names.
// Cross-command merge: whenever a token ends with "…" and the next token (after an operator)
// is a standalone "…", drop the operator and the standalone "…" (absorbed by the trailing "…").
// Also deduplicates consecutive standalone "…" tokens. Applied repeatedly until stable.
func renderModels(models []*subCmdModel, ops []string) string {
	// Build interleaved token list: [cmd0, op0, cmd1, op1, cmd2, ...]
	// Each cmd is its rendered string.
	type piece struct {
		text string
		isOp bool
	}
	var pieces []piece
	for i, m := range models {
		var tokens []string
		if m.leadMarker {
			tokens = append(tokens, "…")
		}
		tokens = append(tokens, cmdDisplayName(m.name))
		// Collect contiguous runs of removed segs; emit "…" only if run is > 1 rune.
		var runTexts []string
		for j, seg := range m.segs {
			if m.removed[j] {
				runTexts = append(runTexts, seg.text)
			} else {
				if len(runTexts) > 0 {
					runText := strings.Join(runTexts, " ")
					if utf8.RuneCountInString(runText) > 1 {
						tokens = append(tokens, "…")
					} else {
						tokens = append(tokens, runTexts...)
					}
					runTexts = runTexts[:0]
				}
				tokens = append(tokens, seg.text)
			}
		}
		// Flush any pending run at end of segs.
		if len(runTexts) > 0 {
			runText := strings.Join(runTexts, " ")
			if utf8.RuneCountInString(runText) > 1 {
				tokens = append(tokens, "…")
			} else {
				tokens = append(tokens, runTexts...)
			}
		}
		part := strings.Join(tokens, " ")
		// Only append trailMarker if the part does not already end with "…"
		// (avoids intra-command "… …" when a removed-seg run ends the tokens).
		if m.trailMarker && !strings.HasSuffix(part, "…") {
			part += " …"
		}
		pieces = append(pieces, piece{part, false})
		if i < len(ops) {
			pieces = append(pieces, piece{ops[i], true})
		}
	}

	// Cross-command collapse: if left cmd ends with "…" and right cmd is exactly "…",
	// drop the operator between them and the standalone "…" (it is covered by left's trailing "…").
	// Repeat until stable.
	for {
		changed := false
		var next []piece
		i := 0
		for i < len(pieces) {
			p := pieces[i]
			// Check pattern: non-op ending in "…", followed by op, followed by standalone "…"
			if !p.isOp && strings.HasSuffix(p.text, "…") &&
				i+2 < len(pieces) && pieces[i+1].isOp && pieces[i+2].text == "…" && !pieces[i+2].isOp {
				// Absorb: keep left cmd, drop op and standalone "…"
				next = append(next, p)
				i += 3
				changed = true
				continue
			}
			next = append(next, p)
			i++
		}
		pieces = next
		if !changed {
			break
		}
	}

	// Deduplicate consecutive standalone "…" cmd pieces (separated by operators).
	for {
		changed := false
		var next []piece
		i := 0
		for i < len(pieces) {
			p := pieces[i]
			// Check: standalone "…" cmd followed by op followed by standalone "…" cmd → merge
			if !p.isOp && p.text == "…" &&
				i+2 < len(pieces) && pieces[i+1].isOp && pieces[i+2].text == "…" && !pieces[i+2].isOp {
				next = append(next, p)
				i += 3
				changed = true
				continue
			}
			next = append(next, p)
			i++
		}
		pieces = next
		if !changed {
			break
		}
	}

	// Join all pieces with spaces.
	var sb strings.Builder
	for i, p := range pieces {
		if i > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteString(p.text)
	}
	// Final pass: collapse any "… …" sequences (ellipses separated only by spaces)
	// into a single "…". Repeat until stable to handle runs of three or more.
	result := sb.String()
	for strings.Contains(result, "… …") {
		result = strings.ReplaceAll(result, "… …", "…")
	}
	return result
}

// reduceToBudget implements command-first graduated omission via layer-jump + restore-to-fill.
// It takes the already noise-stripped parts (alternating cmd/op from splitSubCommands) and
// trims the minimum necessary to fit within maxLen, marking every removed span with "…".
// Algorithm: Stage A (omit all args → restore from ends) → Stage B (omit middle commands →
// restore from ends) → final hard cap via truncateMiddle.
func reduceToBudget(parts []string, maxLen int) string {
	// Separate sub-commands (even indices) and operators (odd indices).
	var cmds []string
	var ops []string
	for i, p := range parts {
		if i%2 == 0 {
			cmds = append(cmds, p)
		} else {
			ops = append(ops, p)
		}
	}
	// Build models.
	models := make([]*subCmdModel, len(cmds))
	for i, cmd := range cmds {
		m := &subCmdModel{}
		// Detect leading "… " or standalone "…" marker.
		work := cmd
		if work == "…" {
			m.name = "…"
			m.removed = []bool{}
			models[i] = m
			continue
		}
		if strings.HasPrefix(work, "… ") {
			m.leadMarker = true
			work = work[len("… "):]
		}
		// Detect trailing " …" from redirection removal.
		if strings.HasSuffix(work, " …") {
			m.trailMarker = true
			work = work[:len(work)-len(" …")]
		}
		tokens := tokenizeCommand(work)
		if len(tokens) == 0 {
			m.name = "…"
			m.removed = []bool{}
			models[i] = m
			continue
		}
		m.name = tokens[0]
		if len(tokens) > 1 {
			m.segs = classifyArgSegments(m.name, tokens[1:])
		}
		m.removed = make([]bool, len(m.segs))
		models[i] = m
	}

	// isHostArg identifies tokens that are host args (user@host) — never truncate these.
	isHostArg := func(text string) bool {
		return strings.Contains(text, "@") && !strings.ContainsAny(text, "\"' ")
	}

	// isDotPlaceholder identifies models that are standalone "…" placeholders.
	isDotPlaceholder := func(m *subCmdModel) bool {
		return m.name == "…" && !m.leadMarker && !m.trailMarker && len(m.segs) == 0
	}

	// Rule 1: check full render.
	full := renderModels(models, ops)
	if compactLen(full) <= maxLen {
		return full
	}

	// Stage A: omit ALL args (flags + plain args) in every model. Keep command names.
	for _, m := range models {
		for j := range m.removed {
			m.removed[j] = true
		}
	}

	// restoreSegsToFill restores segs of model m one by one to fill the budget.
	// Priority: key args (host) first, then short-to-long for everything else.
	// When a seg doesn't fit whole, middle-truncate the LONGEST remaining non-key arg to fill, then STOP.
	restoreSegsToFill := func(m *subCmdModel, snapshot []bool) {
		// Restore from snapshot (all removed).
		copy(m.removed, snapshot)
		// Build restore priority: key args first, then by length ascending (shortest first).
		type segItem struct {
			j      int
			length int
			isKey  bool
		}
		var items []segItem
		for j, seg := range m.segs {
			items = append(items, segItem{j, compactLen(seg.text), isHostArg(seg.text)})
		}
		// Sort: key args first, then shorter first.
		for i := 0; i < len(items)-1; i++ {
			for jj := i + 1; jj < len(items); jj++ {
				a, b := items[i], items[jj]
				if b.isKey && !a.isKey {
					items[i], items[jj] = items[jj], items[i]
				} else if !b.isKey && !a.isKey && b.length < a.length {
					items[i], items[jj] = items[jj], items[i]
				}
			}
		}
		// Restore segs one by one in priority order.
		for _, it := range items {
			m.removed[it.j] = false
			cur := renderModels(models, ops)
			if compactLen(cur) <= maxLen {
				continue // fits, keep restoring
			}
			// This seg causes overflow. If it's a key arg, revert and stop — can't truncate it.
			if it.isKey {
				m.removed[it.j] = true
				return
			}
			// Find the longest non-key restored seg to middle-truncate.
			bestSi := -1
			bestLen := 0
			for j, seg := range m.segs {
				if m.removed[j] || isHostArg(seg.text) {
					continue
				}
				if compactLen(seg.text) > bestLen {
					bestLen = compactLen(seg.text)
					bestSi = j
				}
			}
			if bestSi < 0 {
				m.removed[it.j] = true
				return
			}
			excess := compactLen(cur) - maxLen
			newLen := bestLen - excess
			if newLen < 4 {
				newLen = 4
			}
			origText := m.segs[bestSi].text
			m.segs[bestSi].text = truncateMiddle(origText, newLen)
			if compactLen(renderModels(models, ops)) > maxLen {
				// Still over — revert the truncation attempt and remove the newly-added seg.
				// Keep any segs that were already restored before this iteration.
				m.segs[bestSi].text = origText
				m.removed[it.j] = true
			}
			return // stop after truncation attempt
		}
	}

	stageArender := renderModels(models, ops)
	if compactLen(stageArender) <= maxLen {
		// Stage A fits: restore args from both ends toward the middle to fill budget.
		n := len(models)
		// Build restore order: 0, n-1, 1, n-2, 2, n-3, ...
		order := make([]int, 0, n)
		lo, hi := 0, n-1
		for lo <= hi {
			order = append(order, lo)
			if hi != lo {
				order = append(order, hi)
			}
			lo++
			hi--
		}
		for _, idx := range order {
			m := models[idx]
			if len(m.segs) == 0 {
				continue
			}
			// Save current removed state.
			snapshot := make([]bool, len(m.removed))
			copy(snapshot, m.removed)
			// Try full restore of this command's args.
			for j := range m.removed {
				m.removed[j] = false
			}
			if compactLen(renderModels(models, ops)) <= maxLen {
				// Full restore fits — keep and continue to next command.
				continue
			}
			// Full restore overflows — restore segment-by-segment to fill budget.
			restoreSegsToFill(m, snapshot)
			// Stop after partial restore — budget is now filled or we hit a limit.
			break
		}
		return renderModels(models, ops)
	}

	// Stage A still too long → Stage B.
	// Stage B: omit middle commands (args already all removed).
	// Keep first and last non-placeholder as anchors; mark middles as omitted placeholders.
	// renderModels will collapse adjacent "…" tokens.
	{
		firstAnchor, lastAnchor := -1, -1
		for i, m := range models {
			if !isDotPlaceholder(m) {
				if firstAnchor < 0 {
					firstAnchor = i
				}
				lastAnchor = i
			}
		}
		if firstAnchor < 0 {
			// No real anchors at all.
			out := renderModels(models, ops)
			if compactLen(out) > maxLen {
				out = truncateMiddle(out, maxLen)
			}
			return out
		}

		// Mark all middle models (between anchors) as dot placeholders.
		// Only replace a command with "…" if its rendered text is > 1 rune;
		// a 1-rune span kept verbatim (replacing with the 1-rune "…" saves nothing).
		for i := firstAnchor + 1; i < lastAnchor; i++ {
			if !isDotPlaceholder(models[i]) {
				rendered := renderModels([]*subCmdModel{models[i]}, nil)
				if utf8.RuneCountInString(rendered) > 1 {
					models[i] = &subCmdModel{name: "…", removed: []bool{}}
				}
			}
		}

		stageBrender := renderModels(models, ops)
		if compactLen(stageBrender) <= maxLen {
			// Under-filled: restore from both ends toward the middle.
			// Collect middle indices in order: nearest anchors first, inward.
			var middles []int
			for i := firstAnchor + 1; i < lastAnchor; i++ {
				middles = append(middles, i)
			}
			// Build restore order: outer (near anchors) first, then inward.
			// From left: firstAnchor+1, firstAnchor+2, ...
			// From right: lastAnchor-1, lastAnchor-2, ...
			// Interleave: left then right, inward.
			restoreOrder := make([]int, 0, len(middles))
			ml, mr := 0, len(middles)-1
			for ml <= mr {
				restoreOrder = append(restoreOrder, middles[ml])
				if mr != ml {
					restoreOrder = append(restoreOrder, middles[mr])
				}
				ml++
				mr--
			}
			// We need the original models to restore from. But we replaced them with dots.
			// Rebuild from the original parts.
			origModels := make([]*subCmdModel, len(cmds))
			for i, cmd := range cmds {
				m := &subCmdModel{}
				work := cmd
				if work == "…" {
					m.name = "…"
					m.removed = []bool{}
					origModels[i] = m
					continue
				}
				if strings.HasPrefix(work, "… ") {
					m.leadMarker = true
					work = work[len("… "):]
				}
				if strings.HasSuffix(work, " …") {
					m.trailMarker = true
					work = work[:len(work)-len(" …")]
				}
				tokens := tokenizeCommand(work)
				if len(tokens) == 0 {
					m.name = "…"
					m.removed = []bool{}
					origModels[i] = m
					continue
				}
				m.name = tokens[0]
				if len(tokens) > 1 {
					m.segs = classifyArgSegments(m.name, tokens[1:])
				}
				m.removed = make([]bool, len(m.segs))
				// Keep all args removed for restored models in stage B.
				for j := range m.removed {
					m.removed[j] = true
				}
				origModels[i] = m
			}

			for _, idx := range restoreOrder {
				// Un-omit: replace dot placeholder with the original model (args still removed).
				models[idx] = origModels[idx]
				if compactLen(renderModels(models, ops)) <= maxLen {
					continue
				}
				// Overflows — revert. Only revert to "…" if doing so actually saves runes.
				origRendered := renderModels([]*subCmdModel{origModels[idx]}, nil)
				if utf8.RuneCountInString(origRendered) > 1 {
					models[idx] = &subCmdModel{name: "…", removed: []bool{}}
				}
				// If origRendered is 1 rune, keep the restored model (it can't be shorter).
				break
			}
			return renderModels(models, ops)
		}

		// Even first+last+"…" too long → hard cap.
		out := stageBrender
		if compactLen(out) > maxLen {
			out = truncateMiddle(out, maxLen)
		}
		return out
	}
}

// stripContinuation merges shell line-continuation sequences (\<newline>) into a
// single logical line so downstream ";" joining does not produce bare flags after ";".
func stripContinuation(cmd string) string {
	lines := strings.Split(cmd, "\n")
	var result []string
	cont := false // previous physical line ended with "\"
	for _, line := range lines {
		trimmed := strings.TrimRight(line, " \t")
		isCont := strings.HasSuffix(trimmed, "\\")
		if isCont {
			trimmed = strings.TrimRight(trimmed[:len(trimmed)-1], " \t")
		}
		if cont && len(result) > 0 {
			// Continuation: fold into the previous logical line with a single space.
			piece := strings.TrimLeft(trimmed, " \t")
			if result[len(result)-1] == "" {
				result[len(result)-1] = piece
			} else if piece != "" {
				result[len(result)-1] += " " + piece
			}
		} else {
			result = append(result, trimmed)
		}
		cont = isCont
	}
	return strings.Join(result, "\n")
}

// stripHeredocBody removes heredoc body content from multi-line commands.
// Only activates when << is an actual unquoted shell heredoc operator.
// Appends " …" to the first line to indicate body was omitted.
func stripHeredocBody(cmd string) string {
	firstLine := cmd
	if idx := strings.Index(cmd, "\n"); idx >= 0 {
		firstLine = cmd[:idx]
	} else {
		return cmd
	}
	inSingle, inDouble := false, false
	found := false
	for i, r := range firstLine {
		switch {
		case r == '\'' && !inDouble:
			inSingle = !inSingle
		case r == '"' && !inSingle:
			inDouble = !inDouble
		case r == '<' && !inSingle && !inDouble && i+1 < len(firstLine) && firstLine[i+1] == '<':
			found = true
		}
	}
	if !found {
		return cmd
	}
	return firstLine + " …"
}

// CompactBashCommand applies multi-level simplification to a Bash command for compact display.
// maxLen governs the character (rune) budget; <=0 defaults to compactMaxLen.
func CompactBashCommand(cmd string, maxLen int) string {
	if maxLen <= 0 {
		maxLen = compactMaxLen
	}
	cmd, commentsOmitted := stripShellComments(cmd)
	if cmd == "" {
		if commentsOmitted {
			return "…"
		}
		return "…"
	}
	cmd = stripHeredocBody(cmd)
	cmd = stripContinuation(cmd)
	// Flatten multi-line commands quote-awarely: out-of-quote newline => " ; "; in-quote newline => space.
	cmd = normalizeShellNewlines(cmd)
	result := strings.Join(strings.Fields(cmd), " ")
	// Filter redirection tokens before path compression; track env and redirection omissions
	{
		subCmds := splitSubCommands(result)
		var filtered []string
		for i, part := range subCmds {
			if i%2 == 1 {
				filtered = append(filtered, part)
				continue
			}
			stripped := stripLeadingEnvAssignments(part)
			hadEnv := len(stripped) < len(part)
			tokens := tokenizeCommand(stripped)
			clean, removedRedir := filterRedirections(tokens)
			if len(clean) == 0 {
				// Entire sub-command was env or redirections
				filtered = append(filtered, "…")
				continue
			}
			text := strings.Join(clean, " ")
			if hadEnv {
				text = "… " + text
			}
			// Append removed redirection: use "…" only if removed text is > 1 rune.
			if utf8.RuneCountInString(removedRedir) > 1 {
				text = text + " …"
			} else if removedRedir != "" {
				text = text + " " + removedRedir
			}
			filtered = append(filtered, text)
		}
		filtered = cleanSubCommandParts(filtered)
		result = strings.Join(filtered, " ")
	}
	if result == "" || result == "…" {
		// Prepend comment marker if comments were also removed
		if commentsOmitted && result == "" {
			return "…"
		}
		return "…"
	}
	// Prepend … for removed comments if not already starting with …
	if commentsOmitted && !strings.HasPrefix(result, "…") {
		result = "… " + result
	}
	result = compressPathsInText(result)
	if compactLen(result) <= maxLen {
		return result
	}
	return reduceToBudget(splitSubCommands(result), maxLen)
}

// HeaderInfo contains common header fields for all notification types.
type HeaderInfo struct {
	Project           string
	CWD               string
	TmuxTarget        string
	CLICommand        string
	ContextUsedPct    int // -1 means no data
	ContextUsedTokens int
	ContextWindowSize int
	SendFrom          string // f29: SessionSend sender identity → the 👤 details line
	SendKind          string // f29 G: SessionSend type ("normal"/"no-header") → the 🏷 details line; "" omits it
}

// buildHeader generates the common header lines for notifications.
func buildHeader(firstLine string, h HeaderInfo) []string {
	lines := []string{firstLine}
	// f29: the 📂 folder line is now conditional (was unconditional) — an event with no project/CWD
	// (SessionSend, a CWD-less Cron) no longer renders an empty "📂 " line.
	if pd := projectDisplay(h.Project, h.CWD); pd != "" {
		lines = append(lines, "📂 "+markdown.EscapeHTML(pd))
	}
	// f29: the sender identity renders as a 👤 line inside the metadata/details block (boss: sender in the
	// collapsible block, not appended to the visible status line).
	if h.SendFrom != "" {
		lines = append(lines, "👤 "+markdown.EscapeHTML(h.SendFrom))
	}
	// f29 G: the SessionSend type (normal / no-header) renders as a 🏷 line, after the 👤 sender and before
	// the 📟 pane line. Only SessionSend populates SendKind, so no other event emits this line.
	if h.SendKind != "" {
		lines = append(lines, "🏷 "+markdown.EscapeHTML(h.SendKind))
	}
	if h.TmuxTarget != "" {
		lines = append(lines, "📟 "+markdown.EscapeHTML(FormatPaneID(h.TmuxTarget)))
	}
	if h.CLICommand != "" {
		lines = append(lines, "🖥 "+markdown.EscapeHTML(h.CLICommand))
	}
	if h.ContextUsedPct >= 0 {
		used := float64(h.ContextUsedTokens)
		usedStr := formatTokens(used)
		totalStr := formatTokens(float64(h.ContextWindowSize))
		lines = append(lines, fmt.Sprintf("📊 Context: %d%% (%s/%s)", h.ContextUsedPct, usedStr, totalStr))
	}
	return lines
}

// FormatPaneID returns the full tmux target string as-is.
func FormatPaneID(tmuxTarget string) string {
	return tmuxTarget
}

func formatTokens(v float64) string {
	if v >= 1_000_000 {
		return fmt.Sprintf("%.1fM", v/1_000_000)
	}
	return fmt.Sprintf("%.1fk", v/1000)
}

// projectDisplay returns a display string for the project: compressed CWD if available, else raw project.
func projectDisplay(project, cwd string) string {
	if cwd != "" {
		return CompressPath(cwd)
	}
	return project
}

func BuildNotificationText(data NotificationData) string {
	var emoji, status, sendKind string
	switch {
	case data.Event == "SessionStart":
		emoji = "🟢"
		status = "Session Started"
	case data.Event == "SessionEnd":
		emoji = "🔴"
		status = "Session Ended"
	case data.Event == "PreToolUse":
		emoji = "💬"
		status = "Update"
	case data.Event == "PostToolUse":
		emoji = "🔧"
		status = "Tool Done"
	case data.Event == "ToolUse":
		emoji = "🔧"
		status = data.ToolName
		if status == "" {
			status = "Tool Call"
		}
	case data.Event == "CompactTool":
		emoji = "🔧"
		status = "Tool Activity"
	case data.Event == "Cron":
		// DIALECT-NEUTRAL header: emoji + status only. The rich-only <hr/> at the header/body
		// boundary is added separately by helpers.InsertRichHr; NOT here.
		if data.CronNoHeader {
			emoji = "📨"
			status = "Cron (silent)"
		} else {
			emoji = "🔔"
			status = "Cron"
		}
		jobID := data.CronJobID
		if len(jobID) > 8 {
			jobID = jobID[:8]
		}
		if jobID != "" {
			status += " " + jobID
		}
		if data.CronName != "" {
			status += " (" + markdown.EscapeHTML(data.CronName) + ")"
		}
	case data.Event == "SessionSend":
		// DIALECT-NEUTRAL header: emoji + status only. Rich-only <hr/> via helpers.InsertRichHr.
		// f29 G: ONE unified pen glyph on the visible line (NO "(silent)" suffix); the normal/no-header
		// type moved into a 🏷 details line (HeaderInfo.SendKind). U+FE0F (VS16) makes TG render the pen
		// (U+1F58A, text-default) as a color emoji.
		emoji = "🖊️"
		status = "CLI Send"
		if data.SendNoHeader {
			sendKind = "no-header"
		} else {
			sendKind = "normal"
		}
		// f29: the sender moved into the 👤 details line (HeaderInfo.SendFrom); the visible status line
		// no longer carries a " from <agent>" suffix.
		status += DeliveryStatusTag(data.DeliveryStatus)
	case data.Event == "Message":
		switch {
		case data.Interrupted:
			// Item 7: this streamed bubble's pi run was truncated by a retryable provider error (stream cut,
			// overloaded, 429/5xx) and pi is auto-retrying the SAME turn — a complete answer follows in a new
			// bubble below. Mark the truncated bubble so the half-message is not mistaken for a real answer.
			// Distinct from ✅ Task Completed (done), ⏹ Interrupted (ESC abort), and ⚠️ Run Error (Item 3b,
			// retries exhausted). Precedence over Finalized (an errored bubble is never a Stop-relabel target).
			emoji = "🔄"
			status = "Interrupted — retrying…"
		case data.Finalized:
			// f29 F: the turn-FINAL streamed message (Finalized = the relabeled last chunk, position-based
			// per cmd/stream.go) is labeled "Task Completed" to match the Stop-direct-send; earlier bubbles
			// (Finalized=false) stay "💬 Message".
			emoji = "✅"
			status = "Task Completed"
		default:
			emoji = "💬"
			status = "Message"
		}
	case data.Event == "Stop":
		// f29 F: the Stop-direct-send turn-final message. Same output as the old default, made explicit so
		// the position-based label (turn-final → Task Completed) is documented alongside the Message case.
		emoji = "✅"
		status = "Task Completed"
	case data.Event == "AgentInterrupted":
		// pi ESC-abort (stopReason "aborted"): a distinct header with a STOP glyph — NOT the default
		// "✅ Task Completed", which would misreport an interrupted turn as a completed one.
		emoji = "⏹"
		status = "Interrupted"
	case data.Event == "AgentError":
		emoji = "⚠️"
		status = "Run Error"
	default:
		emoji = "✅"
		status = "Task Completed"
	}
	var statusLine string
	if data.AgentName != "" {
		statusLine = emoji + " [" + markdown.EscapeHTML(data.AgentName) + "] " + status
	} else {
		statusLine = emoji + " " + status
	}
	if data.Page > 0 {
		statusLine += fmt.Sprintf(" (%d/%d)", data.Page, data.TotalPages)
	}
	lines := buildHeader(statusLine, HeaderInfo{
		Project: data.Project, CWD: data.CWD, TmuxTarget: data.TmuxTarget,
		CLICommand: data.CLICommand, ContextUsedPct: data.ContextUsedPct,
		ContextUsedTokens: data.ContextUsedTokens, ContextWindowSize: data.ContextWindowSize,
		SendFrom: data.SendFrom, SendKind: sendKind,
	})
	if data.Body != "" {
		lines = append(lines, "", data.Body)
	}
	return strings.Join(lines, "\n")
}

func HeaderLen(data NotificationData) int {
	d := data
	d.Body = ""
	return len([]rune(BuildNotificationText(d)))
}

func BuildPermissionText(data PermissionData) string {
	firstLine := "🔐 Permission Request"
	if data.AgentName != "" {
		firstLine = "🔐 [" + markdown.EscapeHTML(data.AgentName) + "] Permission Request"
	}
	lines := buildHeader(firstLine, HeaderInfo{
		Project: data.Project, CWD: data.CWD, TmuxTarget: data.TmuxTarget,
		CLICommand: data.CLICommand, ContextUsedPct: -1,
	})
	lines = append(lines, "", "🔧 Tool: "+markdown.EscapeHTML(data.ToolName))
	// Show key fields from tool_input
	for _, key := range []string{"command", "description", "file_path", "old_string", "new_string", "replace_all", "url", "query", "pattern", "prompt"} {
		if v, ok := data.ToolInput[key]; ok {
			s := fmt.Sprintf("%v", v)
			if key == "old_string" || key == "new_string" {
				lines = append(lines, key+":\n<pre>"+markdown.EscapeHTML(s)+"</pre>")
			} else if key == "description" {
				lines = append(lines, "ℹ️ "+markdown.EscapeHTML(s))
			} else {
				lines = append(lines, key+": "+markdown.EscapeHTML(s))
			}
		}
	}
	if data.SuggestionDesc != "" {
		lines = append(lines, "", "💡 Always Allow: "+markdown.EscapeHTML(data.SuggestionDesc))
	}
	return strings.Join(lines, "\n")
}

// buildToolNotifyBody formats the argument body of a tool call (the human-readable args, e.g. the Bash
// command or the Edit old/new blocks) with relevant emojis. Returns (body, parsed): parsed is false
// when tool_input is unparseable (body is then the raw input). Shared by BuildToolNotifyText (which
// wraps it in a 🔧-summary <details>) and BuildCompactToolDetails (Fix 14).
func buildToolNotifyBody(toolName string, toolInput json.RawMessage, cwd string) (string, bool) {
	var fields map[string]interface{}
	if err := json.Unmarshal(toolInput, &fields); err != nil {
		return string(toolInput), false
	}
	var b strings.Builder
	esc := func(v interface{}) string {
		return markdown.EscapeHTML(fmt.Sprintf("%v", v))
	}
	// first returns the first present key's value among aliases, letting a normalised pi tool (CC name, pi
	// field keys) resolve — e.g. Read/Edit/Write use first("file_path", "path") because pi's field is `path`.
	// Symmetric with BuildCompactToolLine's str() fallback (Fix R4 Item 4); the compact one-line summary got
	// the fallback in commit 32bf054 but this details body did not, so a pi Read/Edit/Write rendered no path.
	first := func(keys ...string) (interface{}, bool) {
		for _, key := range keys {
			if v, ok := fields[key]; ok {
				return v, true
			}
		}
		return nil, false
	}
	switch toolName {
	case "Bash":
		if cmd, ok := fields["command"]; ok {
			fmt.Fprintf(&b, "💻 %s", esc(cmd))
		}
		if desc, ok := fields["description"]; ok {
			fmt.Fprintf(&b, "\nℹ️ %s", esc(desc))
		}
		if timeout, ok := fields["timeout"]; ok {
			fmt.Fprintf(&b, "\n⏱️ timeout: %s", esc(timeout))
		}
		if bg, ok := fields["run_in_background"]; ok {
			fmt.Fprintf(&b, "\n🔄 background: %s", esc(bg))
		}
	case "Edit":
		if fp, ok := first("file_path", "path"); ok {
			fmt.Fprintf(&b, "📄 %s", esc(fp))
		}
		if ra, ok := fields["replace_all"]; ok {
			fmt.Fprintf(&b, "\n🔄 replace_all: %s", esc(ra))
		}
		oldStr := ""
		if old, ok := first("old_string", "oldText"); ok {
			oldStr = fmt.Sprintf("%v", old)
		}
		newStr := ""
		if ns, ok := first("new_string", "newText"); ok {
			newStr = fmt.Sprintf("%v", ns)
		}
		if oldStr != "" {
			fmt.Fprintf(&b, "\n\nOld:\n<pre>%s</pre>", markdown.ExpandTabs(esc(oldStr)))
		} else {
			fmt.Fprintf(&b, "\n\nOld: (empty)")
		}
		if newStr != "" {
			fmt.Fprintf(&b, "\n\nNew:\n<pre>%s</pre>", markdown.ExpandTabs(esc(newStr)))
		} else {
			fmt.Fprintf(&b, "\n\nNew: (empty)")
		}
	case "Write":
		if fp, ok := first("file_path", "path"); ok {
			fmt.Fprintf(&b, "📄 %s", esc(fp))
		}
		if content, ok := fields["content"]; ok {
			fmt.Fprintf(&b, "\n\n<pre>%s</pre>", markdown.ExpandTabs(esc(content)))
		}
	case "Read":
		if fp, ok := first("file_path", "path"); ok {
			fmt.Fprintf(&b, "📄 %s", esc(fp))
		}
		if offset, ok := fields["offset"]; ok {
			fmt.Fprintf(&b, "\n📍 offset: %s", esc(offset))
		}
		if limit, ok := fields["limit"]; ok {
			fmt.Fprintf(&b, "\n📏 limit: %s", esc(limit))
		}
	case "Glob":
		if pattern, ok := fields["pattern"]; ok {
			fmt.Fprintf(&b, "🔍 %s", esc(pattern))
		}
		if path, ok := fields["path"]; ok {
			fmt.Fprintf(&b, "\n📂 %s", esc(path))
		}
	case "Grep":
		if pattern, ok := fields["pattern"]; ok {
			fmt.Fprintf(&b, "🔍 %s", esc(pattern))
		}
		if path, ok := fields["path"]; ok {
			fmt.Fprintf(&b, "\n📂 %s", esc(path))
		}
		for _, key := range []string{"output_mode", "glob", "type", "-n", "-B", "-A", "-C", "-i"} {
			if v, ok := fields[key]; ok {
				fmt.Fprintf(&b, "\n%s: %s", key, esc(v))
			}
		}
	case "Agent":
		if desc, ok := fields["description"]; ok {
			fmt.Fprintf(&b, "ℹ️ %s", esc(desc))
		}
		if st, ok := fields["subagent_type"]; ok {
			fmt.Fprintf(&b, "\n🤖 %s", esc(st))
		}
		if model, ok := fields["model"]; ok {
			fmt.Fprintf(&b, "\n🏷️ model: %s", esc(model))
		}
		if bg, ok := fields["run_in_background"]; ok {
			fmt.Fprintf(&b, "\n🔄 background: %s", esc(bg))
		}
		if iso, ok := fields["isolation"]; ok {
			fmt.Fprintf(&b, "\n📦 isolation: %s", esc(iso))
		}
	case "WebFetch":
		if url, ok := fields["url"]; ok {
			fmt.Fprintf(&b, "🌐 %s", esc(url))
		}
		if prompt, ok := fields["prompt"]; ok {
			fmt.Fprintf(&b, "\nℹ️ %s", esc(prompt))
		}
	case "WebSearch":
		if query, ok := fields["query"]; ok {
			fmt.Fprintf(&b, "🔍 %s", esc(query))
		}
	case "AskUserQuestion":
		questions, _ := fields["questions"].([]interface{})
		for qi, q := range questions {
			qMap, _ := q.(map[string]interface{})
			if qMap == nil {
				continue
			}
			if qi > 0 {
				b.WriteString("\n\n")
			}
			header, _ := qMap["header"].(string)
			question, _ := qMap["question"].(string)
			if header != "" {
				fmt.Fprintf(&b, "❓ %s\n", esc(header))
			}
			if question != "" {
				b.WriteString(esc(question))
			}
			options, _ := qMap["options"].([]interface{})
			for oi, o := range options {
				oMap, _ := o.(map[string]interface{})
				if oMap == nil {
					continue
				}
				label, _ := oMap["label"].(string)
				desc, _ := oMap["description"].(string)
				if desc != "" {
					fmt.Fprintf(&b, "\n%d. %s — %s", oi+1, esc(label), esc(desc))
				} else {
					fmt.Fprintf(&b, "\n%d. %s", oi+1, esc(label))
				}
			}
		}
	default:
		// Unknown tool: key: value fallback; complex types use JSON
		for k, v := range fields {
			switch v.(type) {
			case string, float64, bool:
				fmt.Fprintf(&b, "%s: %s\n", k, esc(v))
			default:
				jsonBytes, err := json.MarshalIndent(v, "", "  ")
				if err == nil {
					fmt.Fprintf(&b, "%s:\n<pre>%s</pre>\n", k, markdown.EscapeHTML(string(jsonBytes)))
				} else {
					fmt.Fprintf(&b, "%s: %s\n", k, esc(v))
				}
			}
		}
	}
	return b.String(), true
}

// BuildToolNotifyText formats a full tool call notification: the args body wrapped in a collapsed
// <details> block with a "🔧 <tool>" summary. A no-arg tool (empty body) returns a name-only skeleton
// so the notification is still sent (Fix 13a); an unparseable tool_input returns the raw text.
func BuildToolNotifyText(toolName string, toolInput json.RawMessage, cwd string) string {
	body, parsed := buildToolNotifyBody(toolName, toolInput, cwd)
	if !parsed {
		return body
	}
	if body == "" {
		return "🔧 " + markdown.EscapeRich(toolName)
	}
	// Wrap args in a collapsed <details> block; tapping expands to show tool args.
	return fmt.Sprintf("<details><summary>🔧 %s</summary>\n%s\n</details>", markdown.EscapeRich(toolName), body)
}

// BuildCompactToolDetails wraps the compact one-line summary (<=maxLen runes) in a collapsed <details>
// block whose body is the full tool-call args, so the compact rich notification can be expanded to see
// the actual command (Fix 14). A no-arg / unparseable tool has no expandable body → just the summary line.
func BuildCompactToolDetails(toolName string, toolInput json.RawMessage, cwd string, maxLen int) string {
	summary := BuildCompactToolLine(toolName, toolInput, cwd, maxLen)
	body, parsed := buildToolNotifyBody(toolName, toolInput, cwd)
	if !parsed || body == "" {
		return summary
	}
	return "<details><summary>" + summary + "</summary>\n" + body + "\n</details>"
}

// BuildCompactToolLine returns a single-line compact description of a tool call.
// Format: "ToolName: brief_info" (HTML-escaped for use in HTML-mode messages).
// maxLen governs the character (rune) budget for the variable content portion; <=0 defaults to compactMaxLen.
func BuildCompactToolLine(toolName string, toolInput json.RawMessage, cwd string, maxLen int) string {
	if maxLen <= 0 {
		maxLen = compactMaxLen
	}
	var fields map[string]interface{}
	if err := json.Unmarshal(toolInput, &fields); err != nil {
		return markdown.EscapeHTML(toolName)
	}
	// str returns the first present key's value. The fallback list lets a normalised pi tool (CC name, but pi
	// field keys) resolve: e.g. Read/Edit/Write use str("file_path", "path") since pi's field is `path`.
	str := func(keys ...string) string {
		for _, key := range keys {
			if v, ok := fields[key]; ok {
				return fmt.Sprintf("%v", v)
			}
		}
		return ""
	}
	truncate := func(s string, max int) string {
		if utf8.RuneCountInString(s) <= max {
			return s
		}
		limit := max - 1 // reserve 1 rune for "…"
		if limit <= 0 {
			r := []rune(s)
			return string(r[:max])
		}
		pos, runes := 0, 0
		for pos < len(s) && runes < limit {
			_, size := utf8.DecodeRuneInString(s[pos:])
			pos += size
			runes++
		}
		return s[:pos] + "…"
	}
	var emoji string
	switch toolName {
	case "Read":
		emoji = "📖"
	case "Edit":
		emoji = "✏️"
	case "Write":
		emoji = "📝"
	case "Bash":
		emoji = "💻"
	case "ls":
		// pi's `ls` (kept lowercase — no CC analogue; see NormalizePiToolName).
		emoji = "📂"
	case "Glob", "Grep":
		emoji = "🔍"
	case "Agent":
		emoji = "🤖"
	case "WebFetch":
		emoji = "🌐"
	case "WebSearch":
		emoji = "🔎"
	case "Skill":
		emoji = "⚡"
	case "MCP":
		emoji = "🔌"
	case "TaskCreate", "TaskUpdate", "TaskGet", "TaskList", "TaskStop", "TaskOutput":
		emoji = "📋"
	case "NotebookEdit", "EnterPlanMode", "ExitPlanMode", "EnterWorktree", "ExitWorktree":
		emoji = "⚙️"
	default:
		emoji = "🔧"
	}
	var info string
	switch toolName {
	case "Read":
		// Fix 17: compact Read shows only the filename (basename) — the full path is in the collapsed
		// details body. TailPath(...,1) returns the last path segment. pi's field is `path` (fallback).
		info = truncate(TailPath(str("file_path", "path"), 1), maxLen)
	case "Edit", "Write":
		info = truncate(TailPath(str("file_path", "path"), 3), maxLen)
	case "ls":
		// pi `ls` (lowercase): show the listed directory path (field `path`; empty = cwd).
		info = truncate(TailPath(str("path"), 3), maxLen)
	case "Bash":
		info = CompactBashCommand(str("command"), maxLen)
	case "Glob":
		info = truncate(str("pattern"), maxLen)
	case "Grep":
		info = str("pattern")
		if p := str("path"); p != "" {
			info += " in " + TailPath(p, 3)
		}
		info = truncate(info, maxLen)
	case "Agent":
		model := str("model")
		subtype := str("subagent_type")
		desc := str("description")
		var prefix string
		switch {
		case model != "" && subtype != "":
			prefix = "Agent(" + model + "/" + subtype + ")"
		case model != "":
			prefix = "Agent(" + model + ")"
		case subtype != "":
			prefix = "Agent(" + subtype + ")"
		default:
			info = truncate(desc, maxLen)
		}
		if prefix != "" {
			prefixLen := utf8.RuneCountInString(prefix) + 2 // ": " is 2 runes
			remaining := maxLen - prefixLen
			if remaining < 1 {
				remaining = 1
			}
			body := prefix
			if desc != "" {
				body = prefix + ": " + truncate(desc, remaining)
			}
			return emoji + " " + markdown.EscapeHTML(truncate(body, maxLen))
		}
	case "WebFetch":
		info = truncate(str("url"), maxLen)
	case "WebSearch":
		info = truncate(str("query"), maxLen)
	case "Skill":
		info = truncate(str("skill"), maxLen)
	default:
		for k, v := range fields {
			info = fmt.Sprintf("%s=%v", k, v)
			break
		}
		info = truncate(info, maxLen)
	}
	if info == "" {
		return emoji + " " + markdown.EscapeHTML(toolName)
	}
	return emoji + " " + markdown.EscapeHTML(toolName) + ": " + markdown.EscapeHTML(info)
}

// BuildToolResultText formats a tool result for appending to PostToolUse notifications.
// Bash results show stdout and stderr separately; other tools show all fields.
func BuildToolResultText(toolName string, toolResponse json.RawMessage) string {
	if len(toolResponse) == 0 {
		return "✅ Done"
	}
	var strResult string
	if err := json.Unmarshal(toolResponse, &strResult); err == nil {
		return "✅ Result:\n<pre>" + markdown.EscapeHTML(strResult) + "</pre>"
	}
	// pi tool results are an ARRAY of content blocks — [{"type":"text","text":"…"}] (verified in pi 0.84.1
	// transcripts + bot.log). Neither the string unmarshal above nor the map unmarshal below matches an array,
	// so without this branch a pi result falls through to the raw <pre> JSON dump. Join the text blocks and
	// render like the plain-string case. A CC map result (object) fails this array unmarshal and falls through
	// to the map branch unchanged.
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(toolResponse, &blocks); err == nil && len(blocks) > 0 {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		if len(parts) > 0 {
			return "✅ Result:\n<pre>" + markdown.EscapeHTML(strings.Join(parts, "\n")) + "</pre>"
		}
	}
	var fields map[string]interface{}
	if err := json.Unmarshal(toolResponse, &fields); err == nil {
		if toolName == "Bash" {
			var b strings.Builder
			b.WriteString("✅ Result:\n")
			if stdout, ok := fields["stdout"]; ok {
				s := fmt.Sprintf("%v", stdout)
				if s != "" {
					fmt.Fprintf(&b, "<pre>%s</pre>", markdown.EscapeHTML(s))
				}
			}
			if stderr, ok := fields["stderr"]; ok {
				s := fmt.Sprintf("%v", stderr)
				if s != "" {
					fmt.Fprintf(&b, "\nstderr:\n<pre>%s</pre>", markdown.EscapeHTML(s))
				}
			}
			result := b.String()
			if result == "✅ Result:\n" {
				return "✅ Done"
			}
			return result
		}
		if toolName == "AskUserQuestion" {
			var b strings.Builder
			b.WriteString("✅ Result:\n")
			if answers, ok := fields["answers"].(map[string]interface{}); ok {
				for q, a := range answers {
					fmt.Fprintf(&b, "%s → %s\n", markdown.EscapeHTML(fmt.Sprintf("%v", q)), markdown.EscapeHTML(fmt.Sprintf("%v", a)))
				}
			}
			result := b.String()
			if result == "✅ Result:\n" {
				return "✅ Done"
			}
			return result
		}
		var b strings.Builder
		b.WriteString("✅ Result:\n")
		for k, v := range fields {
			switch v.(type) {
			case string, float64, bool:
				fmt.Fprintf(&b, "%s: %s\n", k, markdown.EscapeHTML(fmt.Sprintf("%v", v)))
			default:
				jsonBytes, err := json.MarshalIndent(v, "", "  ")
				if err == nil {
					fmt.Fprintf(&b, "%s:\n<pre>%s</pre>\n", k, markdown.EscapeHTML(string(jsonBytes)))
				} else {
					fmt.Fprintf(&b, "%s: %s\n", k, markdown.EscapeHTML(fmt.Sprintf("%v", v)))
				}
			}
		}
		return b.String()
	}
	return "✅ Result:\n<pre>" + markdown.EscapeHTML(string(toolResponse)) + "</pre>"
}

func BuildQuestionText(data QuestionData) string {
	firstLine := "❓ Question"
	if data.AgentName != "" {
		firstLine = "❓ [" + markdown.EscapeHTML(data.AgentName) + "] Question"
	}
	lines := buildHeader(firstLine, HeaderInfo{
		Project: data.Project, CWD: data.CWD, TmuxTarget: data.TmuxTarget,
		CLICommand: data.CLICommand, ContextUsedPct: data.ContextUsedPct,
		ContextUsedTokens: data.ContextUsedTokens, ContextWindowSize: data.ContextWindowSize,
	})
	if len(data.Questions) > 1 {
		for qIdx, q := range data.Questions {
			multiTag := ""
			if q.MultiSelect {
				multiTag = " (多选)"
			}
			lines = append(lines, "", fmt.Sprintf("<b>Q%d: %s</b>%s", qIdx+1, markdown.EscapeHTML(q.Header), multiTag))
			lines = append(lines, markdown.EscapeHTML(q.Question))
			for i, opt := range q.Options {
				lines = append(lines, fmt.Sprintf("%d. %s", i+1, markdown.EscapeHTML(opt.Label)))
				if opt.Description != "" {
					lines = append(lines, "  → "+markdown.EscapeHTML(opt.Description))
				}
			}
		}
	} else if len(data.Questions) == 1 {
		q := data.Questions[0]
		if q.Header != "" {
			lines = append(lines, "", "📋 "+markdown.EscapeHTML(q.Header))
		}
		lines = append(lines, "", markdown.EscapeHTML(q.Question))
		for i, opt := range q.Options {
			lines = append(lines, fmt.Sprintf("%d. %s", i+1, markdown.EscapeHTML(opt.Label)))
			if opt.Description != "" {
				lines = append(lines, "  → "+markdown.EscapeHTML(opt.Description))
			}
		}
	} else {
		if data.Header != "" {
			lines = append(lines, "", "📋 "+markdown.EscapeHTML(data.Header))
		}
		lines = append(lines, "", markdown.EscapeHTML(data.Question))
		for i, opt := range data.Options {
			lines = append(lines, fmt.Sprintf("%d. %s", i+1, markdown.EscapeHTML(opt.Label)))
			if opt.Description != "" {
				lines = append(lines, "  → "+markdown.EscapeHTML(opt.Description))
			}
		}
	}
	return strings.Join(lines, "\n")
}
