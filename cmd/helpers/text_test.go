package helpers

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// TestFindUnclosedTagsEmptyPseudoTags verifies that empty or whitespace-only
// pseudo-tags ("<>", "< >", "<\t>") do not panic and are treated as non-tags.
// Regression for the production crash at text.go:37 (strings.Fields(tag)[0]
// index out of range when tag is empty/whitespace).
func TestFindUnclosedTagsEmptyPseudoTags(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty tag", "<>", nil},
		{"space tag", "< >", nil},
		{"tab tag", "<\t>", nil},
		{"empty tag among text", "a <> b", nil},
		{"empty tag before real tag", "<><b>hi", []string{"b"}},
		{"unclosed b", "<b>hello", []string{"b"}},
		{"balanced b", "<b>hi</b>", nil},
		{"unclosed code", "<code>x", []string{"code"}},
		{"non-whitelist tag", "a < z > c", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := findUnclosedTags(c.in)
			// slices.Equal treats nil and empty slices as equal; findUnclosedTags
			// can return a non-nil empty slice (append-then-remove), so
			// reflect.DeepEqual(got, nil) would falsely fail here.
			if !slices.Equal(got, c.want) {
				t.Errorf("findUnclosedTags(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// TestSplitBodyEmptyTagNoPanic reproduces the production crash path: a body
// longer than maxRuneLen containing an empty pseudo-tag "<>" must split without
// panicking (previously panicked in findUnclosedTags via SplitBody).
func TestSplitBodyEmptyTagNoPanic(t *testing.T) {
	body := "<>" + strings.Repeat("a", 50)
	chunks := SplitBody(body, 10)
	if len(chunks) == 0 {
		t.Fatal("SplitBody returned no chunks")
	}
	// No newlines and no real HTML tags in body, so chunks must rejoin to the
	// exact original content (no dropped separators, no inserted tags).
	if joined := strings.Join(chunks, ""); joined != body {
		t.Errorf("SplitBody altered content:\n got: %q\nwant: %q", joined, body)
	}
}

// TestSplitBodyNoSplitInsideTag verifies SplitBody never cuts inside an HTML
// tag. With a hard split (no newlines) landing inside "</code>", the old code
// produced malformed HTML like "</cod</code>" that Telegram rejected (400).
func TestSplitBodyNoSplitInsideTag(t *testing.T) {
	body := "<code>" + strings.Repeat("X", 12) + "</code>" // 25 runes
	chunks := SplitBody(body, 23)                           // hard split lands at "...<code>XXXXXXXXXXXX</cod"
	if len(chunks) < 2 {
		t.Fatalf("expected the body to be split into >=2 chunks, got %d: %q", len(chunks), chunks)
	}
	xCount := 0
	for i, c := range chunks {
		if strings.Contains(c, "</cod</code>") {
			t.Errorf("chunk %d contains malformed tag %q: %q", i, "</cod</code>", c)
		}
		// No truncated tag: every '<' must have a '>' after it in the chunk.
		for off := 0; off < len(c); off++ {
			if c[off] == '<' {
				if !strings.Contains(c[off:], ">") {
					t.Errorf("chunk %d has a truncated tag (a '<' with no later '>'): %q", i, c)
					break
				}
			}
		}
		// Each chunk must be self-balanced (SplitBody closes any unclosed tag).
		if got := findUnclosedTags(c); !slices.Equal(got, nil) {
			t.Errorf("chunk %d has unclosed tags %v: %q", i, got, c)
		}
		xCount += strings.Count(c, "X")
	}
	if xCount != 12 {
		t.Errorf("content not preserved: total X = %d, want 12 (chunks=%q)", xCount, chunks)
	}
}

// --- TC4: SplitBody rich + block-count tests ---

// TestSplitBodyRichUnderThreshold verifies that a rich body just under 32768 runes returns 1 chunk.
func TestSplitBodyRichUnderThreshold(t *testing.T) {
	// 32760 runes of plain text — well under the 32768 threshold
	body := strings.Repeat("a", 32760)
	chunks := SplitBody(body, 32768)
	if len(chunks) != 1 {
		t.Errorf("expected 1 chunk for body under threshold, got %d", len(chunks))
	}
}

// TestSplitBodyRichOverThreshold verifies that a rich body just over 32768 runes returns >=2 chunks,
// and no chunk ends inside an open <table> or <details> tag.
func TestSplitBodyRichOverThreshold(t *testing.T) {
	// Build a body that exceeds 32768 runes with balanced <table> tags
	// Each row is 50 runes: "<tr><td>" + 40 chars + "</td></tr>\n"
	var sb strings.Builder
	sb.WriteString("<table>")
	rowCount := 700 // 700 * ~50 runes >> 32768
	for i := 0; i < rowCount; i++ {
		sb.WriteString("<tr><td>")
		sb.WriteString(strings.Repeat("x", 40))
		sb.WriteString("</td></tr>\n")
	}
	sb.WriteString("</table>")
	body := sb.String()

	if len([]rune(body)) <= 32768 {
		t.Fatalf("test body must exceed 32768 runes, got %d", len([]rune(body)))
	}

	chunks := SplitBody(body, 32768)
	if len(chunks) < 2 {
		t.Fatalf("expected >=2 chunks for body over threshold, got %d", len(chunks))
	}

	// No chunk should end inside an open <table> or <details> (check tag balancing)
	for i, c := range chunks {
		unclosed := findUnclosedTags(c)
		// unclosed tags are closed by SplitBody — so the chunk itself must be self-balanced
		if len(unclosed) > 0 {
			t.Errorf("chunk %d has unclosed tags %v: %.80q...", i, unclosed, c)
		}
		if len([]rune(c)) > 32768 {
			t.Errorf("chunk %d exceeds 32768 runes: %d", i, len([]rune(c)))
		}
	}
}

// TestSplitBodyBlockCountTable verifies TC4 C1:
// A ~600-row table body that is UNDER 32768 characters but has >500 <tr> blocks
// → SplitBody splits by block count (chunk count > 1), NOT by character count.
func TestSplitBodyBlockCountTable(t *testing.T) {
	// Build 600 rows of <tr><td>x</td></tr> — each row is ~22 chars, 600*22 = ~13200 < 32768
	// but 600 <tr> tags > richBlockLimit(500).
	var sb strings.Builder
	sb.WriteString("<table>")
	for i := 0; i < 600; i++ {
		sb.WriteString("<tr><td>x</td></tr>")
	}
	sb.WriteString("</table>")
	body := sb.String()

	runeLen := len([]rune(body))
	if runeLen >= 32768 {
		t.Fatalf("test body must be under 32768 runes, got %d", runeLen)
	}
	blockCount := countBlocks(body)
	if blockCount <= richBlockLimit {
		t.Fatalf("test body must have >%d blocks, got %d", richBlockLimit, blockCount)
	}

	chunks := SplitBody(body, 32768)
	if len(chunks) < 2 {
		t.Errorf("expected split by block count (>=2 chunks), got %d chunk(s); body has %d runes, %d blocks",
			len(chunks), runeLen, blockCount)
	}
	// Each chunk must fit within the block limit
	for i, c := range chunks {
		cb := countBlocks(c)
		if cb > richBlockLimit {
			t.Errorf("chunk %d still has %d blocks > limit %d", i, cb, richBlockLimit)
		}
	}
}

// TestSplitBodyBlockCountList verifies TC4 C1 for lists:
// 600 <li> items under 32768 chars → splits by block count.
func TestSplitBodyBlockCountList(t *testing.T) {
	// Each item: "<li>item NNN</li>" ~16 chars; 600*16 = ~9600 < 32768
	// But <ul>+<li> blocks: <ul>=1, 600*<li>=600, total 601 > 500.
	var sb strings.Builder
	sb.WriteString("<ul>")
	for i := 0; i < 600; i++ {
		sb.WriteString(fmt.Sprintf("<li>item %d</li>", i))
	}
	sb.WriteString("</ul>")
	body := sb.String()

	runeLen := len([]rune(body))
	if runeLen >= 32768 {
		t.Fatalf("test body must be under 32768 runes, got %d", runeLen)
	}
	blockCount := countBlocks(body)
	if blockCount <= richBlockLimit {
		t.Fatalf("test body must have >%d blocks, got %d", richBlockLimit, blockCount)
	}

	chunks := SplitBody(body, 32768)
	if len(chunks) < 2 {
		t.Errorf("expected split by block count (>=2 chunks) for 600 <li>, got %d chunk(s); body has %d runes, %d blocks",
			len(chunks), runeLen, blockCount)
	}
	for i, c := range chunks {
		cb := countBlocks(c)
		if cb > richBlockLimit {
			t.Errorf("chunk %d still has %d blocks > limit %d", i, cb, richBlockLimit)
		}
	}
}

// TestCountBlocksBasic verifies countBlocks counts block-level opening tags correctly.
func TestCountBlocksBasic(t *testing.T) {
	cases := []struct {
		html string
		want int
	}{
		{"<li>item</li>", 1},
		{"<tr><td>x</td></tr>", 1},           // only <tr> is a block tag, not <td>
		{"<ul><li>a</li><li>b</li></ul>", 3}, // <ul> + 2 <li>
		{"<blockquote>q</blockquote>", 1},
		{"<details><summary>s</summary></details>", 1}, // only <details> is a block tag
		{"<pre>code</pre>", 1},
		{"no tags here", 0},
		{"<b>bold</b>", 0}, // inline tag, not counted
	}
	for _, c := range cases {
		got := countBlocks(c.html)
		if got != c.want {
			t.Errorf("countBlocks(%q) = %d, want %d", c.html, got, c.want)
		}
	}
}

// TestSplitBody21ColTable verifies that SplitBody does not panic on a 21-column table
// (the 20-column guard is not yet implemented in Phase 1 — we assert no crash and valid output).
func TestSplitBody21ColTable(t *testing.T) {
	// Build a single-row table with 21 columns
	var sb strings.Builder
	sb.WriteString("<table><tr>")
	for i := 0; i < 21; i++ {
		sb.WriteString("<td>cell</td>")
	}
	sb.WriteString("</tr></table>")
	body := sb.String()

	// Must not panic; must return at least one non-empty chunk
	chunks := SplitBody(body, 32768)
	if len(chunks) == 0 {
		t.Fatal("SplitBody returned no chunks for 21-column table")
	}
	total := strings.Join(chunks, "")
	if !strings.Contains(total, "<td>cell</td>") {
		t.Errorf("content was lost after SplitBody of 21-column table")
	}
}
