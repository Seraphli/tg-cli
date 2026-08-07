package helpers

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/Seraphli/tg-cli/internal/markdown"
	"github.com/Seraphli/tg-cli/internal/notify"
)

// htmlTags lists Telegram-supported HTML tags that need open/close tracking.
// Includes both legacy tags and rich dialect block/inline tags.
var htmlTags = []string{
	"b", "i", "code", "pre", "s", "a", "blockquote",
	"h1", "h2", "h3", "h4", "h5", "h6",
	"table", "tr", "td", "th",
	"details", "summary",
	"ul", "ol", "li",
	"aside", "footer",
	"mark", "sub", "sup", "tg-spoiler",
}

// findUnclosedTags returns a list of unclosed tag names (in open order) in s.
func findUnclosedTags(s string) []string {
	var stack []string
	i := 0
	for i < len(s) {
		if s[i] != '<' {
			i++
			continue
		}
		end := strings.Index(s[i:], ">")
		if end < 0 {
			break
		}
		tag := s[i+1 : i+end]
		i += end + 1
		closing := strings.HasPrefix(tag, "/")
		if closing {
			name := strings.ToLower(strings.TrimSpace(tag[1:]))
			for j := len(stack) - 1; j >= 0; j-- {
				if stack[j] == name {
					stack = append(stack[:j], stack[j+1:]...)
					break
				}
			}
		} else {
			// Self-closing or unknown — only track known Telegram tags.
			// Empty or whitespace-only pseudo-tags (e.g. "<>", "< >") produce
			// no fields; skip them to avoid an index-out-of-range panic.
			fields := strings.Fields(tag)
			if len(fields) == 0 {
				continue
			}
			name := strings.ToLower(fields[0])
			for _, t := range htmlTags {
				if name == t {
					stack = append(stack, name)
					break
				}
			}
		}
	}
	return stack
}

// closingTags returns closing HTML tags for the given open tag names (reverse order).
func closingTags(open []string) string {
	var b strings.Builder
	for i := len(open) - 1; i >= 0; i-- {
		fmt.Fprintf(&b, "</%s>", open[i])
	}
	return b.String()
}

// openingTags returns opening HTML tags for the given tag names.
func openingTags(open []string) string {
	var b strings.Builder
	for _, t := range open {
		fmt.Fprintf(&b, "<%s>", t)
	}
	return b.String()
}

// blockTags are tags that count toward the 500-block limit (inclusive per the API spec).
// Each opening <li>, <tr>, <blockquote>, <details>, and any block element = 1.
var blockTags = map[string]bool{
	"li": true, "tr": true, "blockquote": true, "details": true,
	"p": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"pre": true, "ul": true, "ol": true, "table": true, "aside": true, "footer": true,
}

// countBlocks counts block elements (inclusive) toward the 500-block API limit.
// Each opening tag in blockTags counts as 1.
func countBlocks(html string) int {
	count := 0
	s := html
	for {
		lt := strings.Index(s, "<")
		if lt < 0 {
			break
		}
		gt := strings.Index(s[lt:], ">")
		if gt < 0 {
			break
		}
		tag := s[lt+1 : lt+gt]
		s = s[lt+gt+1:]
		if strings.HasPrefix(tag, "/") || strings.HasPrefix(tag, "!") {
			continue
		}
		// Extract tag name (first field, strip attributes)
		fields := strings.Fields(tag)
		if len(fields) == 0 {
			continue
		}
		name := strings.ToLower(strings.TrimRight(fields[0], "/"))
		if blockTags[name] {
			count++
		}
	}
	return count
}

// richBlockLimit is the maximum block count per message (inclusive, per API spec).
const richBlockLimit = 500

// chunkFits returns true when s satisfies both the rune limit and the block count limit.
func chunkFits(s string, maxRuneLen int) bool {
	return len([]rune(s)) <= maxRuneLen && countBlocks(s) <= richBlockLimit
}

// SplitBody splits body text into chunks fitting within maxRuneLen runes AND within the
// richBlockLimit block count (500 inclusive). Tries to split at paragraph boundaries (\n\n),
// then line boundaries (\n), falling back to hard rune-boundary split.
// Checks for unclosed HTML tags after each split and appends/prepends closing/opening tags.
// The threshold sourced from cfg.RichMaxRunes should be passed as maxRuneLen by callers.
func SplitBody(body string, maxRuneLen int) []string {
	runes := []rune(body)
	if chunkFits(body, maxRuneLen) {
		return []string{body}
	}
	var chunks []string
	for len(runes) > 0 {
		current := string(runes)
		if chunkFits(current, maxRuneLen) {
			chunks = append(chunks, current)
			break
		}
		// Use the smaller of maxRuneLen and the remaining rune count as the window
		window := maxRuneLen
		if window > len(runes) {
			window = len(runes)
		}
		chunk := string(runes[:window])
		var end int
		var skip int
		if idx := strings.LastIndex(chunk, "\n\n"); idx > 0 {
			end = len([]rune(chunk[:idx]))
			skip = 2
		} else if idx := strings.LastIndex(chunk, "\n"); idx > 0 {
			end = len([]rune(chunk[:idx]))
			skip = 1
		} else {
			end = window
			skip = 0
		}
		// Never split inside an HTML tag: if the split point falls between a '<'
		// and its closing '>', back up to before the '<' so a tag is never cut
		// in half (a half-cut tag like "</cod" makes findUnclosedTags mis-close
		// it into malformed HTML that Telegram rejects with a 400).
		for k := end - 1; k >= 0; k-- {
			if runes[k] == '>' {
				break // nearest delimiter is '>': split is outside any tag
			}
			if runes[k] == '<' {
				if k > 0 {
					end = k
					skip = 0
				}
				break
			}
		}
		// If block count of the candidate chunk still exceeds the limit, find a
		// smaller split point by walking back until countBlocks fits.
		// Split point is always AFTER a '>' or '\n' so the part is never cut mid-tag.
		for end > 1 && countBlocks(string(runes[:end])) > richBlockLimit {
			// Walk backward past current position to find the end of a complete tag ('>') or a newline.
			// We stop when runes[end-1] is '>' or '\n', meaning runes[:end] ends cleanly.
			end--
			for end > 1 && runes[end-1] != '\n' && runes[end-1] != '>' {
				end--
			}
			skip = 0
		}
		if end <= 0 {
			// Emergency fallback: take at least 1 rune to avoid an infinite loop
			end = 1
			skip = 0
		}
		part := string(runes[:end])
		unclosed := findUnclosedTags(part)
		reopen := ""
		if len(unclosed) > 0 {
			reopen = openingTags(unclosed)
		}
		// Balanced-split path: close open tags on this chunk and reopen them on the next — but ONLY
		// when reopening actually shrinks the remainder. The length check is a forward-progress guard:
		// for raw/unescaped content whose tag-like sequences are not real HTML (e.g. a large terminal
		// capture containing <b>/<td>), the reopen tags can grow without bound and consume no input,
		// looping forever (the production /p hang). In that case fall through to emit the part as-is
		// and advance by end+skip, which strictly shrinks the remainder and guarantees termination.
		if reopen != "" && len([]rune(reopen)) < end+skip {
			chunks = append(chunks, part+closingTags(unclosed))
			runes = []rune(reopen + string(runes[end+skip:]))
		} else {
			chunks = append(chunks, part)
			runes = runes[end+skip:]
		}
	}
	return chunks
}

// TruncateStr truncates s to at most maxRunes runes, appending "..." if truncated.
func TruncateStr(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) > maxRunes {
		return string(r[:maxRunes]) + "..."
	}
	return s
}

// ShortenSeparators replaces long separator lines with a short 3-char version.
func ShortenSeparators(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		runes := []rune(trimmed)
		if len(runes) < 10 {
			continue
		}
		sepCount := 0
		for _, r := range runes {
			switch {
			case r >= 0x2500 && r <= 0x257F:
				sepCount++
			case r == '-' || r == '=' || r == '_':
				sepCount++
			}
		}
		if sepCount*100/len(runes) >= 80 {
			firstSep := runes[0]
			lines[i] = string([]rune{firstSep, firstSep, firstSep})
		}
	}
	return strings.Join(lines, "\n")
}

// legacyMaxRunes is the Telegram parse_mode=HTML message limit (4096 runes, inclusive).
const legacyMaxRunes = 4096

// splitWorstCaseArchiveID is a fixed 12-digit sentinel archive ID used ONLY to size the header for the
// fits() upper bound: FinalizeRichHTMLWithID injects "🆔 #<id>" into the collapsed metadata block, so
// the widest realistic id must be reserved when checking whether a page fits under richMax.
const splitWorstCaseArchiveID = int64(999999999999)

// InsertRichHr inserts a rich-dialect "<hr/>" at the header/body boundary — the FIRST blank-line
// separator ("\n\n") that follows the header block. BuildNotificationText joins the header lines and the
// body with a single "" line (rendered as "\n\n"), so the first "\n\n" is exactly that boundary. The
// "<hr/>" is a rich-only horizontal rule between the neutral header and the body; RichifyNewlines later
// collapses a redundant "<hr/><br>". Applied ONLY to Cron/SessionSend RICH notifications; NEVER to the
// legacy HTML fallback. No-op when the text has no "\n\n" boundary (header-only, empty body).
func InsertRichHr(text string) string {
	idx := strings.Index(text, "\n\n")
	if idx < 0 {
		return text
	}
	return text[:idx] + "\n\n<hr/>" + text[idx+len("\n\n"):]
}

// SplitRichLegacyBodyPages is the paired raw-Markdown page builder for Cron/SessionSend rich
// notifications. Unlike SplitBody (which operates on already-rendered HTML), it splits the RAW Markdown
// body into pages such that EACH page, when wrapped in the neutral BuildNotificationText header and
// rendered in BOTH dialects, fits: the rich payload ≤ richMax runes AND ≤ 500 blocks after the full
// send-time finalize (InsertRichHr + FinalizeRichHTMLWithID with a worst-case archive id), and the legacy
// payload ≤ 4096 runes. Returns EQUAL-LENGTH paired BODY chunks: chunks[i] = RenderRichHTML(page_i),
// legacyChunks[i] = RenderTelegramHTML(page_i). The header/marker is added by the caller at wrap time.
//
// Single pass: the pagination "(N/M)" marker widens the header, so the fits() check uses ndMaxMarker —
// nd with Page/TotalPages set to an upper bound on the page count — so the header width is fixed upfront
// and never grows as pages accumulate. Units are paragraphs (\n\n), then lines (\n) inside an oversized
// paragraph; a fenced code block that overflows is re-fenced and hard-split; a newline-free oversized
// unit is reduce-then-validated with a fits(firstRawRune) guard (returns an error if even the first rune
// alone cannot fit — a header too large to leave room for any body).
func SplitRichLegacyBodyPages(rawBody string, nd notify.NotificationData, richMax int) (chunks, legacyChunks []string, err error) {
	// Upper bound on the page count: a page holds at least 1 rune of body, so pages ≤ body-runes + 1.
	// A minimum of 1 keeps a valid marker when the body is empty.
	upper := utf8.RuneCountInString(rawBody) + 1
	if upper < 1 {
		upper = 1
	}
	ndMaxMarker := nd
	ndMaxMarker.Page = upper
	ndMaxMarker.TotalPages = upper
	// fits reports whether a single page carrying rawPage as its RAW Markdown body fits both dialects.
	fits := func(rawPage string) bool {
		nr := ndMaxMarker
		nr.Body = markdown.RenderRichHTML(rawPage)
		richHTML := FinalizeRichHTMLWithID(InsertRichHr(notify.BuildNotificationText(nr)), splitWorstCaseArchiveID)
		if utf8.RuneCountInString(richHTML) > richMax {
			return false
		}
		if countBlocks(richHTML) > richBlockLimit {
			return false
		}
		nl := ndMaxMarker
		nl.Body = markdown.RenderTelegramHTML(rawPage)
		legacyHTML := notify.BuildNotificationText(nl)
		return utf8.RuneCountInString(legacyHTML) <= legacyMaxRunes
	}
	// PRE-CHECK: the header alone (empty body) must fit — else no body can ever be placed.
	if !fits("") {
		return nil, nil, fmt.Errorf("notification header too large to fit an empty body")
	}
	// Break the body into ordered units. A unit is a paragraph; an oversized paragraph is broken into
	// line units; an oversized fenced-code unit is re-fenced and hard-split; an oversized newline-free
	// unit is reduced-then-validated.
	units, uerr := splitBodyUnits(rawBody, fits)
	if uerr != nil {
		return nil, nil, uerr
	}
	// Greedily accumulate units into pages under the fits() budget.
	var pages []string
	var cur string
	for _, u := range units {
		if cur == "" {
			cur = u
			continue
		}
		candidate := cur + "\n\n" + u
		if fits(candidate) {
			cur = candidate
			continue
		}
		pages = append(pages, cur)
		cur = u
	}
	if cur != "" || len(pages) == 0 {
		pages = append(pages, cur)
	}
	for _, p := range pages {
		chunks = append(chunks, markdown.RenderRichHTML(p))
		legacyChunks = append(legacyChunks, markdown.RenderTelegramHTML(p))
	}
	return chunks, legacyChunks, nil
}

// splitBodyUnits breaks a raw Markdown body into ordered, individually-fitting units. Paragraph-first
// (\n\n), then line units (\n) inside an oversized paragraph, then a hard split for an oversized
// newline-free unit (fenced code re-fenced, otherwise reduce-then-validate rune-by-rune with a
// fits(firstRawRune) guard). Every returned unit satisfies fits(unit).
func splitBodyUnits(rawBody string, fits func(string) bool) ([]string, error) {
	var units []string
	for _, para := range strings.Split(rawBody, "\n\n") {
		if fits(para) {
			units = append(units, para)
			continue
		}
		// Oversized paragraph: break into line units.
		for _, line := range strings.Split(para, "\n") {
			if fits(line) {
				units = append(units, line)
				continue
			}
			// Oversized newline-free unit: hard-split (fenced-aware) with reduce-then-validate.
			sub, err := hardSplitUnit(line, fits)
			if err != nil {
				return nil, err
			}
			units = append(units, sub...)
		}
	}
	return units, nil
}

// hardSplitUnit reduce-then-validates a single oversized newline-free unit into a sequence of
// individually-fitting sub-units. A fenced code block (opening ``` on the unit) is re-fenced so each
// sub-unit stays valid Markdown code; a plain unit is split rune-by-rune. The fits(firstRawRune) guard
// returns an error when even the first rune alone cannot fit (header leaves no body room).
func hardSplitUnit(unit string, fits func(string) bool) ([]string, error) {
	// Detect a fenced code block: the unit begins with a ``` (or ~~~) fence (optionally with a language
	// tag). When fenced, split the INNER code content (fence markers stripped) and re-fence each piece so
	// no piece carries a stray original fence marker.
	fence, lang, content := detectFence(unit)
	rewrap := func(s string) string {
		if fence == "" {
			return s
		}
		return fence + lang + "\n" + s + "\n" + fence
	}
	runes := []rune(content)
	if len(runes) == 0 {
		return []string{rewrap("")}, nil
	}
	// fits(firstRawRune) guard: if a single rune cannot fit, the header is too large for any body.
	if !fits(rewrap(string(runes[:1]))) {
		return nil, fmt.Errorf("notification header too large to fit even a one-rune body")
	}
	var subs []string
	i := 0
	for i < len(runes) {
		// Grow the window one rune at a time, re-fencing when inside a code block, until adding the next
		// rune would overflow; then flush. Reduce-then-validate: we always keep the largest prefix that
		// still fits (validated by fits() including the header + marker + worst-case archive id).
		lo := i
		best := i // best is the exclusive end of the largest fitting prefix (at least lo+1)
		for hi := i + 1; hi <= len(runes); hi++ {
			if fits(rewrap(string(runes[lo:hi]))) {
				best = hi
				continue
			}
			break
		}
		if best <= lo {
			best = lo + 1 // forward-progress guard (fits(firstRawRune) already validated one rune fits)
		}
		subs = append(subs, rewrap(string(runes[lo:best])))
		i = best
	}
	return subs, nil
}

// detectFence inspects a single newline-free unit for an opening code fence ("```" or "~~~"). When
// fenced, it returns the fence marker, the leading language token, and the INNER code content (opening
// fence + language + a trailing closing fence stripped) so the content can be split and re-fenced. When
// not fenced it returns ("", "", unit) so the caller splits the unit verbatim. This unit is a single
// newline-free line, so a fenced block here is a one-line ```lang code``` or an unterminated fence.
func detectFence(unit string) (fence, lang, content string) {
	for _, f := range []string{"```", "~~~"} {
		if !strings.HasPrefix(unit, f) {
			continue
		}
		rest := strings.TrimPrefix(unit, f)
		rest = strings.TrimSuffix(rest, f) // drop a trailing closing fence if present
		// The leading token (no spaces) is the language/info string; the remainder is the code content.
		lang, code := "", rest
		if sp := strings.IndexByte(rest, ' '); sp >= 0 {
			lang = rest[:sp]
			code = strings.TrimPrefix(rest[sp:], " ")
		} else {
			lang = rest
			code = ""
		}
		return f, lang, code
	}
	return "", "", unit
}
