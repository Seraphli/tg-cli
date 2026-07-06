package helpers

import (
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
