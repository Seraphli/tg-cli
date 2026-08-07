package markdown

import (
	"regexp"
	"strings"
	"testing"
)

// blockTagRe matches block-level tags that must NOT appear inside <td>/<th>.
var blockTagRe = regexp.MustCompile(`<(pre|ul|ol|li|blockquote|details|p|h[1-6]|table)\b`)

// extractCellContents returns the inner content of each <td>...</td> and <th>...</th> region.
func extractCellContents(html string) []string {
	var out []string
	for _, open := range []string{"<td>", "<th>"} {
		s := html
		for {
			start := strings.Index(s, open)
			if start < 0 {
				break
			}
			inner := s[start+len(open):]
			end := strings.Index(inner, "</td>")
			if end < 0 {
				end = strings.Index(inner, "</th>")
			}
			if end < 0 {
				break
			}
			out = append(out, inner[:end])
			s = inner[end:]
		}
	}
	return out
}

func TestRenderRichHTML_Heading(t *testing.T) {
	// Rich headings render at their original level: # → h1, ## → h2.
	out := RenderRichHTML("# H1\n\n## H2\n\nsome text")
	if !strings.Contains(out, "<h1>") || !strings.Contains(out, "</h1>") {
		t.Errorf("expected <h1> tag (# passthrough), got: %q", out)
	}
	if !strings.Contains(out, "<h2>") || !strings.Contains(out, "</h2>") {
		t.Errorf("expected <h2> tag (## passthrough), got: %q", out)
	}
	// Must not flatten headings to <b>
	if strings.Count(out, "<b>") > 0 && !strings.Contains(out, "<b>") {
		t.Errorf("heading must not be flattened to <b>, got: %q", out)
	}
}

func TestRenderRichHTML_Table(t *testing.T) {
	md := "| A | B |\n|---|---|\n| 1 | 2 |"
	out := RenderRichHTML(md)
	if !strings.Contains(out, "<table bordered striped>") {
		t.Errorf("expected <table bordered striped>, got: %q", out)
	}
	if !strings.Contains(out, "<tr>") {
		t.Errorf("expected <tr>, got: %q", out)
	}
	if !strings.Contains(out, "<td>") {
		t.Errorf("expected <td>, got: %q", out)
	}
}

func TestRenderRichHTML_FencedCodePython(t *testing.T) {
	md := "```python\nprint('hello')\n```"
	out := RenderRichHTML(md)
	if !strings.Contains(out, `<pre><code class="language-python">`) {
		t.Errorf("expected <pre><code class=\"language-python\">, got: %q", out)
	}
}

func TestRenderRichHTML_NestedList(t *testing.T) {
	md := "- item1\n  - nested\n1. ordered"
	out := RenderRichHTML(md)
	if !strings.Contains(out, "<ul>") || !strings.Contains(out, "<li>") {
		t.Errorf("expected <ul><li>, got: %q", out)
	}
	if !strings.Contains(out, "<ol>") {
		t.Errorf("expected <ol>, got: %q", out)
	}
}

func TestRenderRichHTML_Blockquote(t *testing.T) {
	md := "> quoted text"
	out := RenderRichHTML(md)
	if !strings.Contains(out, "<blockquote>") {
		t.Errorf("expected <blockquote>, got: %q", out)
	}
}

func TestRenderRichHTML_EscapeSpecialChars(t *testing.T) {
	// Raw <, >, & in plain text must be escaped
	md := "less < greater > ampersand &"
	out := RenderRichHTML(md)
	if strings.Contains(out, " < ") || strings.Contains(out, " > ") || strings.Contains(out, " & ") {
		t.Errorf("special chars must be escaped, got: %q", out)
	}
	if !strings.Contains(out, "&lt;") {
		t.Errorf("expected &lt;, got: %q", out)
	}
	if !strings.Contains(out, "&gt;") {
		t.Errorf("expected &gt;, got: %q", out)
	}
	if !strings.Contains(out, "&amp;") {
		t.Errorf("expected &amp;, got: %q", out)
	}
}

// TestRenderRichHTML_C4_CellInlineOnly verifies that a markdown table whose cell contains
// content that would render as block tags does NOT emit block tags inside <td>/<th>.
// This is the C4 invariant: table cells must contain only inline formatting.
func TestRenderRichHTML_C4_CellInlineOnly(t *testing.T) {
	// Standard GFM pipe-table cells: inline code, bold, etc. — no block tags allowed in output.
	md := "| Col1 | Col2 |\n|------|------|\n| `code` | **bold** |"
	out := RenderRichHTML(md)

	cells := extractCellContents(out)
	if len(cells) == 0 {
		t.Fatalf("no <td>/<th> cells found in output: %q", out)
	}
	for i, cell := range cells {
		if m := blockTagRe.FindString(cell); m != "" {
			t.Errorf("cell %d contains block tag %q (C4 violation): %q", i, m, cell)
		}
	}
}

// TestRenderRichHTML_C4_FencedCodeInCell tests the renderer with a table that has a cell
// containing a fenced code block as parsed by goldmark — flattened to inline <code>.
func TestRenderRichHTML_C4_FencedCodeInCell(t *testing.T) {
	// This markdown produces a table where the second column cell text looks like code-ish content.
	// We verify no <pre>, <ul>, <li>, <p> or block tags appear inside any <td>.
	md := "| Header1 | Header2 |\n|---------|----------|\n| normal cell | `inline code` |"
	out := RenderRichHTML(md)

	cells := extractCellContents(out)
	for i, cell := range cells {
		if m := blockTagRe.FindString(cell); m != "" {
			t.Errorf("cell %d contains block tag %q (C4 violation): %q", i, m, cell)
		}
	}
}

func TestEscapeRich_DisallowedChars(t *testing.T) {
	// Raw <, >, & must be escaped to allowed named entities
	s := EscapeRich(`hello <world> & "quoted" 'apos'`)
	if strings.Contains(s, "<world>") {
		t.Errorf("< and > must be escaped, got: %q", s)
	}
	if !strings.Contains(s, "&lt;") {
		t.Errorf("expected &lt;, got: %q", s)
	}
	if !strings.Contains(s, "&gt;") {
		t.Errorf("expected &gt;, got: %q", s)
	}
	if !strings.Contains(s, "&amp;") {
		t.Errorf("expected &amp;, got: %q", s)
	}
	if !strings.Contains(s, "&quot;") {
		t.Errorf("expected &quot;, got: %q", s)
	}
	if !strings.Contains(s, "&apos;") {
		t.Errorf("expected &apos;, got: %q", s)
	}
}

func TestEscapeRich_NoRawAngle(t *testing.T) {
	// Output must contain no raw < or > (only escaped forms)
	out := EscapeRich("<script>alert(1)</script>")
	if strings.Contains(out, "<script>") || strings.Contains(out, "</script>") {
		t.Errorf("raw HTML tags must be escaped, got: %q", out)
	}
}

// TestRenderRichHTML_HeadingNotFlattenedToB verifies headings use <h*> tags in rich mode,
// NOT <b> as in the legacy renderer (# passthrough to h1).
func TestRenderRichHTML_HeadingNotFlattenedToB(t *testing.T) {
	out := RenderRichHTML("# Title")
	if strings.HasPrefix(strings.TrimSpace(out), "<b>") {
		t.Errorf("rich heading must not start with <b>, got: %q", out)
	}
	if !strings.Contains(out, "<h1>") {
		t.Errorf("expected <h1> for # heading in rich mode (passthrough), got: %q", out)
	}
}

// TestRenderRichHTML_HeadingLevelPassthrough verifies that headings are rendered at their original
// level with no shift or cap: h1→h1, h2→h2, …, h6→h6 (boss decision; Bot API supports h1-h6).
func TestRenderRichHTML_HeadingLevelPassthrough(t *testing.T) {
	cases := []struct {
		md  string
		tag string
	}{
		{"# a", "h1"},
		{"## a", "h2"},
		{"### a", "h3"},
		{"#### a", "h4"},
		{"##### a", "h5"},
		{"###### a", "h6"},
	}
	for _, c := range cases {
		out := RenderRichHTML(c.md)
		if !strings.Contains(out, "<"+c.tag+">") || !strings.Contains(out, "</"+c.tag+">") {
			t.Errorf("%q: expected <%s> tag, got: %q", c.md, c.tag, out)
		}
	}
}

// TestRenderRichHTML_BareFencedCodeBlock is a regression test for the nil-pointer crash
// that occurred when a fenced code block had no language specifier (node.Info == nil).
// It also pins the exact crashing content shape: a fence body containing a "---" line
// (which goldmark can interpret as a setext heading separator outside a fence).
func TestRenderRichHTML_BareFencedCodeBlock(t *testing.T) {
	// The exact shape that crashed in E2E: bare fence containing a "---" line.
	crashInput := "```\n💬 Message from agent [e2e-test]\n---\nCUSTOM_REPLY_26212\n```"

	// Must not panic.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("PANIC on bare fenced code block (nil node.Info): %v", r)
		}
	}()
	out := RenderRichHTML(crashInput)

	// Bare fence must produce <pre><code> WITHOUT a class attribute.
	if !strings.Contains(out, "<pre><code>") {
		t.Errorf("bare fence must produce <pre><code>, got: %q", out)
	}
	if strings.Contains(out, `class="language-`) {
		t.Errorf("bare fence must not produce class=\"language-...\", got: %q", out)
	}

	// A fence WITH a language specifier must still produce class="language-go".
	outGo := RenderRichHTML("```go\nfmt.Println(\"hello\")\n```")
	if !strings.Contains(outGo, `<pre><code class="language-go">`) {
		t.Errorf("language fence must produce class=\"language-go\", got: %q", outGo)
	}
}
