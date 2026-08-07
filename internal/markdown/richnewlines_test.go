package markdown

import (
	"strings"
	"testing"
)

// T2: RichifyNewlines converts \n -> <br> OUTSIDE <pre>/<tg-math-block>, preserves them INSIDE,
// and is idempotent.
func TestRichifyNewlines_PreservesProtectedRegions(t *testing.T) {
	in := "header a\nheader b\n<pre>code\n\tindent\nend</pre>\nfooter\n<tg-math-block>x\ny</tg-math-block>\ntail"
	out := RichifyNewlines(in)

	// Outside protected regions: newlines become <br>.
	if !strings.Contains(out, "header a<br>header b<br>") {
		t.Errorf("outside-region newlines not converted: %q", out)
	}
	if !strings.HasSuffix(out, "<br>tail") {
		t.Errorf("trailing outside newline not converted: %q", out)
	}
	// Inside <pre>: newlines preserved verbatim, no <br> injected.
	if !strings.Contains(out, "<pre>code\n\tindent\nend</pre>") {
		t.Errorf("inside-<pre> newlines corrupted: %q", out)
	}
	preSeg := out[strings.Index(out, "<pre>"):strings.Index(out, "</pre>")]
	if strings.Contains(preSeg, "<br>") {
		t.Errorf("<br> injected inside <pre>: %q", preSeg)
	}
	// Inside <tg-math-block>: newlines preserved.
	if !strings.Contains(out, "<tg-math-block>x\ny</tg-math-block>") {
		t.Errorf("inside-<tg-math-block> newlines corrupted: %q", out)
	}
	// Idempotent: re-applying is a no-op (remaining \n live only inside protected regions).
	if again := RichifyNewlines(out); again != out {
		t.Errorf("RichifyNewlines not idempotent:\n first=%q\n again=%q", out, again)
	}
}

// Change 1: RichifyNewlines drops the <br> redundant next to a <details> boundary — before an opening
// <details, after a closing </details>, after a </summary> — but KEEPS the <br> before a closing
// </details> and between plain text. Input uses \n separators exactly as the generators emit them.
func TestRichifyNewlines_StripsBrAroundDetails(t *testing.T) {
	in := "🔧 Tool Activity\n<details><summary>📋 C:44%</summary>\n🆔 #117\n📂 cwd\n</details>\n<details><summary>⚡ Skill</summary>\nweb-info\n</details>\nbody line"
	out := RichifyNewlines(in)
	// Removed adjacencies.
	for _, bad := range []string{"<br><details", "</details><br>", "</summary><br>"} {
		if strings.Contains(out, bad) {
			t.Errorf("expected no %q in output: %q", bad, out)
		}
	}
	// Kept: <br> before a CLOSING </details>, and plain-text <br>.
	if !strings.Contains(out, "cwd<br></details>") {
		t.Errorf("<br> before closing </details> must be kept: %q", out)
	}
	if !strings.Contains(out, "web-info<br></details>") {
		t.Errorf("<br> before closing </details> (2nd block) must be kept: %q", out)
	}
	// Collapsed boundaries.
	if !strings.Contains(out, "Tool Activity<details>") {
		t.Errorf("status line should hug the opening <details>: %q", out)
	}
	if !strings.Contains(out, "</summary>🆔 #117") {
		t.Errorf("first detail line should hug the </summary>: %q", out)
	}
	if !strings.Contains(out, "</details><details>") {
		t.Errorf("adjacent details should have no <br> between: %q", out)
	}
	// Idempotent.
	if again := RichifyNewlines(out); again != out {
		t.Errorf("strip not idempotent:\n first=%q\n again=%q", out, again)
	}
}

// Fix 3: RichifyNewlines drops the <br> immediately AFTER an <hr/> (the self-breaking rule needs no
// following line break), so the mailbox body sits directly under the separator. The <br> BEFORE the
// <hr/> is KEPT so the rule stays on its own line below the Subject. Idempotent.
func TestRichifyNewlines_StripsBrAfterHr(t *testing.T) {
	in := "<b>Subject: Hi</b>\n<hr/>\nbody line one\nbody line two"
	out := RichifyNewlines(in)
	if strings.Contains(out, "<hr/><br>") {
		t.Errorf("expected no <hr/><br> (br after the rule must be dropped): %q", out)
	}
	if !strings.Contains(out, "<hr/>body line one") {
		t.Errorf("body should hug the <hr/>: %q", out)
	}
	// The <br> before the <hr/> (line break under the Subject) is kept.
	if !strings.Contains(out, "</b><br><hr/>") {
		t.Errorf("<br> before <hr/> must be kept: %q", out)
	}
	if again := RichifyNewlines(out); again != out {
		t.Errorf("strip not idempotent:\n first=%q\n again=%q", out, again)
	}
}

// T1 (markdown body): multi-paragraph and soft-line-break markdown must render with block/<br>
// separators, never a bare \n that rich HTML would collapse to a space.
func TestRenderRichHTML_MultiLineUsesBrOrBlocks(t *testing.T) {
	// Two paragraphs -> wrapped in <p> (block separation), no bare \n between them.
	multi := RenderRichHTML("para one\n\npara two")
	if strings.Contains(multi, "\n") {
		t.Errorf("two-paragraph rich output has a bare \\n (would collapse): %q", multi)
	}
	if !strings.Contains(multi, "<p>") {
		t.Errorf("two-paragraph rich output missing <p> block separation: %q", multi)
	}
	// Single paragraph with a soft line break -> <br>, and NOT wrapped in <p> (sole block stays inline).
	soft := RenderRichHTML("line one\nline two")
	if strings.Contains(soft, "\n") {
		t.Errorf("soft-break rich output has a bare \\n (would collapse): %q", soft)
	}
	if !strings.Contains(soft, "line one<br>line two") {
		t.Errorf("soft break not rendered as <br>: %q", soft)
	}
	if strings.Contains(soft, "<p>") {
		t.Errorf("sole paragraph should NOT be <p>-wrapped (breaks inline label usage): %q", soft)
	}
}

// T3: rich renderer must NOT leave stray newlines after block elements (they would become <br><br>
// once RichifyNewlines runs). The only newlines allowed in rich output live inside <pre>.
func TestRenderRichHTML_NoStrayNewlineOutsidePre(t *testing.T) {
	out := RenderRichHTML("# Heading\n\nsome text\n\n- item one\n- item two\n\n```\ncode line\n```\n\n| A | B |\n|---|---|\n| 1 | 2 |")
	outside := richPreserveRe.ReplaceAllString(out, "")
	if strings.Contains(outside, "\n") {
		t.Errorf("rich output has a stray \\n outside <pre> (would become <br><br>): outside=%q\nfull=%q", outside, out)
	}
	// Sanity: real block tags are present (heading/list/table/code), not <b>/plain.
	for _, tag := range []string{"<h1>", "<ul>", "<li>", "<pre>", "<table bordered striped>"} {
		if !strings.Contains(out, tag) {
			t.Errorf("expected rich block tag %s in output: %q", tag, out)
		}
	}
}

// T3b: a code block's internal blank lines are preserved (the legacy \n\n\n squeeze must not run on
// the rich path).
func TestRenderRichHTML_CodeBlankLinesPreserved(t *testing.T) {
	out := RenderRichHTML("```\na\n\n\nb\n```")
	if !strings.Contains(out, "a\n\n\nb") {
		t.Errorf("code block blank lines were squeezed on the rich path: %q", out)
	}
}

// T4 (G3): the legacy renderer is untouched by the rich fix — it still emits <b> headings and bare
// \n line breaks, and NONE of the rich-only constructs (<p>, <br>, <h1>).
func TestRenderTelegramHTML_LegacyUnchanged_G3(t *testing.T) {
	legacy := RenderTelegramHTML("# Heading\n\nline one\nline two\n\npara two")
	if !strings.Contains(legacy, "<b>Heading</b>") {
		t.Errorf("legacy heading not <b>: %q", legacy)
	}
	if !strings.Contains(legacy, "\n") {
		t.Errorf("legacy output lost its bare \\n line breaks: %q", legacy)
	}
	for _, richTag := range []string{"<p>", "<br>", "<h1>"} {
		if strings.Contains(legacy, richTag) {
			t.Errorf("legacy output must not contain rich-only tag %s: %q", richTag, legacy)
		}
	}
}
