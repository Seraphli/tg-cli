package helpers

import (
	"fmt"
	"strings"
)

// htmlTags lists Telegram-supported HTML tags that need open/close tracking.
var htmlTags = []string{"b", "i", "code", "pre", "s", "a", "blockquote"}

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

// SplitBody splits body text into chunks fitting within maxRuneLen.
// Tries to split at paragraph boundaries (\n\n), then line boundaries (\n),
// falling back to hard rune-boundary split.
// Checks for unclosed HTML tags after each split and appends/prepends closing/opening tags.
func SplitBody(body string, maxRuneLen int) []string {
	runes := []rune(body)
	if len(runes) <= maxRuneLen {
		return []string{body}
	}
	var chunks []string
	for len(runes) > 0 {
		if len(runes) <= maxRuneLen {
			chunks = append(chunks, string(runes))
			break
		}
		chunk := string(runes[:maxRuneLen])
		var end int
		var skip int
		if idx := strings.LastIndex(chunk, "\n\n"); idx > 0 {
			end = len([]rune(chunk[:idx]))
			skip = 2
		} else if idx := strings.LastIndex(chunk, "\n"); idx > 0 {
			end = len([]rune(chunk[:idx]))
			skip = 1
		} else {
			end = maxRuneLen
			skip = 0
		}
		part := string(runes[:end])
		unclosed := findUnclosedTags(part)
		if len(unclosed) > 0 {
			chunks = append(chunks, part+closingTags(unclosed))
			runes = []rune(openingTags(unclosed) + string(runes[end+skip:]))
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
