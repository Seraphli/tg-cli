package markdown

import (
	"strings"
	"testing"
)

// A full notification header (status + 📂/📟/🖥 + 📊 Context + body) collapses ALL metadata into a
// <details> block (Fix 12): the <summary> is the compact context status, the 📂/📟/🖥 location lines move
// INSIDE first, then the full 📊 Context line last (matching the unfolded header order); only the status
// line and body stay OUTSIDE.
func TestCollapseSessionMeta_WrapsHeaderMetadata(t *testing.T) {
	in := "✅ Task Completed\n📂 /path\n📟 %8@x\n🖥 claude\n📊 Context: 5%\n\nbody"
	// The blank separator line between the collapsed header and the body is dropped (no more \n\n).
	want := "✅ Task Completed\n<details><summary>📋 C:5%</summary>\n📂 /path\n📟 %8@x\n🖥 claude\n📊 Context: 5%\n</details>\nbody"
	if got := CollapseSessionMeta(in); got != want {
		t.Errorf("CollapseSessionMeta mismatch:\n got=%q\nwant=%q", got, want)
	}
}

// Only 📂 present (no 📟/🖥): just that line is wrapped.
func TestCollapseSessionMeta_OnlyCwd(t *testing.T) {
	in := "✅ Done\n📂 /path\n\nbody"
	want := "✅ Done\n<details><summary>📋 Session</summary>\n📂 /path\n</details>\nbody"
	if got := CollapseSessionMeta(in); got != want {
		t.Errorf("CollapseSessionMeta mismatch:\n got=%q\nwant=%q", got, want)
	}
}

// Fix 12: 📊 Context now moves INSIDE the collapsed block (before </details>), and the <summary> is the
// compact context status (📋 C:<...>) rather than the fixed 📋 Session label.
func TestCollapseSessionMeta_ContextInsideAndCompactSummary(t *testing.T) {
	got := CollapseSessionMeta("💬 Update\n📂 /p\n📟 %1@s\n📊 Context: 9%\n\nx")
	closeIdx := strings.Index(got, "</details>")
	ctx := strings.Index(got, "📊 Context")
	if closeIdx < 0 || ctx < 0 || ctx > closeIdx {
		t.Errorf("📊 Context should appear INSIDE the block (before </details>); got=%q", got)
	}
	if !strings.Contains(got, "<summary>📋 C:9%</summary>") {
		t.Errorf("summary should be the compact context status 📋 C:9%%; got=%q", got)
	}
}

// The <summary> extracts the compact context status from the full 📊 Context line, preserving the token
// format: 📋 C:<pct> (<used>/<total>). The full line stays inside the details block.
func TestCollapseSessionMeta_CompactSummaryWithTokens(t *testing.T) {
	got := CollapseSessionMeta("🟢 Idle\n📂 /path\n📟 %14@sock\n🖥 claude\n📊 Context: 39% (315.9k/800.0k)\n\nHello")
	if !strings.Contains(got, "<summary>📋 C:39% (315.9k/800.0k)</summary>") {
		t.Errorf("summary should be 📋 C:39%% (315.9k/800.0k); got=%q", got)
	}
	if i, j := strings.Index(got, "📊 Context: 39% (315.9k/800.0k)"), strings.Index(got, "</details>"); i < 0 || i > j {
		t.Errorf("full 📊 Context line must be inside the details block; got=%q", got)
	}
}

// The blank separator line between the collapsed <details> header and the body is removed — the body
// follows </details> immediately (no wasted vertical space now that the header is collapsed).
func TestCollapseSessionMeta_NoBlankLineAfterDetails(t *testing.T) {
	got := CollapseSessionMeta("💬 Update\n📂 /p\n📟 %1@s\n📊 Context: 9%\n\nStep 1")
	if strings.Contains(got, "</details>\n\n") {
		t.Errorf("blank line after </details> should be removed; got=%q", got)
	}
	if !strings.Contains(got, "</details>\nStep 1") {
		t.Errorf("body should immediately follow </details>; got=%q", got)
	}
}

// rev-16 B.5 + f29: the cron lead emojis (🔔 / 📨) are in notificationLeadEmojis, so a Cron notification
// WITH a CWD (line 1 "📂 ") COLLAPSES its metadata into a <details> block. (SessionSend has no folder line
// post-f29 and anchors on its 👤 sender line — see TestCollapseSessionMeta_SessionSendCollapsesOnPersonAnchor;
// a CWD-less Cron no longer collapses — see TestCollapseSessionMeta_PaneLeadNotCollapsed.)
func TestCollapseSessionMeta_CronWrapped(t *testing.T) {
	for _, in := range []string{
		"🔔 Cron abc123\n📂 /some/cwd\n\nbody",
		"📨 Cron (silent) def456\n📂 /some/cwd\n\nbody",
	} {
		got := CollapseSessionMeta(in)
		if got == in {
			t.Errorf("cron header with a CWD should be collapsed (lead emoji is in the allowlist):\n in=%q", in)
			continue
		}
		// Status line stays on line 0; the metadata is wrapped in a <details> block; 📂 moves inside.
		if !strings.Contains(got, "<details><summary>📋 Session</summary>") {
			t.Errorf("expected collapsed <details> block with 📋 Session summary; got=%q", got)
		}
		if i, j := strings.Index(got, "📂 /some/cwd"), strings.Index(got, "</details>"); i < 0 || j < 0 || i > j {
			t.Errorf("📂 location line must be inside the details block; got=%q", got)
		}
		// The body stays outside the collapsed header.
		if !strings.Contains(got, "</details>\nbody") {
			t.Errorf("body should immediately follow </details>; got=%q", got)
		}
	}
}

// f29 G: a SessionSend notification has NO folder line (no project/CWD); its metadata run anchors on the
// 👤 sender line at index 1 and collapses the 👤 + 🏷 type + 📟 lines into the <details> block.
func TestCollapseSessionMeta_SessionSendCollapsesOnPersonAnchor(t *testing.T) {
	for _, in := range []string{
		"🖊️ CLI Send\n👤 note\n🏷 normal\n📟 %9\n\nbody",
		"🖊️ CLI Send\n👤 note\n🏷 no-header\n📟 %9\n\nbody",
	} {
		got := CollapseSessionMeta(in)
		if got == in {
			t.Errorf("SessionSend header should collapse on the 👤 anchor:\n in=%q", in)
			continue
		}
		if !strings.Contains(got, "<details><summary>📋 Session</summary>") {
			t.Errorf("expected collapsed <details> block with 📋 Session summary; got=%q", got)
		}
		// f29 G: the 👤 sender, 🏷 type, AND 📟 pane lines all move INSIDE the details block.
		for _, meta := range []string{"👤 note", "🏷 ", "📟 %9"} {
			if i, j := strings.Index(got, meta), strings.Index(got, "</details>"); i < 0 || j < 0 || i > j {
				t.Errorf("%q must be inside the details block; got=%q", meta, got)
			}
		}
		if !strings.Contains(got, "</details>\nbody") {
			t.Errorf("body should immediately follow </details>; got=%q", got)
		}
	}
}

// f29 anchor restriction (Delta 1/2): a notification whose line 1 is a 📟 pane line (NOT 📂/👤) is NEVER
// collapsed — even though its lead emoji is in notificationLeadEmojis. Covers cmd/bot_helpers.go:544
// ("✅ Injected …\n📟 …") and a CWD-less Cron (no folder, no sender → pane at line 1, renders UNFOLDED).
// Asserts BYTE-FOR-BYTE unchanged output through BOTH CollapseSessionMeta and CollapseSessionMetaWithID.
func TestCollapseSessionMeta_PaneLeadNotCollapsed(t *testing.T) {
	for _, in := range []string{
		"✅ Injected [abc123] (2)\n📟 %9\nqueued text here", // bot_helpers.go:544 success edit
		"🔔 Cron abc123\n📟 %3\n\nbody",                     // f29: CWD-less Cron renders UNFOLDED
	} {
		if got := CollapseSessionMeta(in); got != in {
			t.Errorf("pane-lead notification must be byte-for-byte unchanged:\n got=%q\n in=%q", got, in)
		}
		if got := CollapseSessionMetaWithID(in, 777); got != in {
			t.Errorf("pane-lead notification must be byte-for-byte unchanged through WithID too:\n got=%q\n in=%q", got, in)
		}
		if strings.Contains(CollapseSessionMeta(in), "<details>") {
			t.Errorf("pane-lead notification must not gain a details block: in=%q", in)
		}
	}
}

// Feature 2: CollapseSessionMetaWithID(msgID>0) inserts a "🆔 #<id>" line first inside the <details>
// block (before the 📂/📟/🖥 location lines); msgID==0 (and plain CollapseSessionMeta) omit it.
func TestCollapseSessionMetaWithID_AddsIDLine(t *testing.T) {
	in := "🟢 Idle\n📂 /path\n📟 %14@sock\n🖥 claude\n📊 Context: 39%\n\nHello"
	got := CollapseSessionMetaWithID(in, 42)
	if !strings.Contains(got, "🆔 #42") {
		t.Errorf("expected 🆔 #42 line; got=%q", got)
	}
	// The 🆔 line must be inside the details block and BEFORE the 📂 location line.
	id := strings.Index(got, "🆔 #42")
	summary := strings.Index(got, "</summary>")
	cwd := strings.Index(got, "📂 /path")
	closeIdx := strings.Index(got, "</details>")
	if !(summary < id && id < cwd && cwd < closeIdx) {
		t.Errorf("🆔 line must sit after <summary>, before 📂, inside <details>; got=%q", got)
	}
	// msgID==0 and the plain wrapper must NOT add the line.
	if strings.Contains(CollapseSessionMetaWithID(in, 0), "🆔") {
		t.Errorf("msgID==0 should not add an 🆔 line")
	}
	if strings.Contains(CollapseSessionMeta(in), "🆔") {
		t.Errorf("CollapseSessionMeta should not add an 🆔 line")
	}
}

// A message whose line 1 is not a folder (📂) or sender (👤) line (e.g. a stream body, or a location-emoji
// lookalike deeper in the body) is unchanged.
func TestCollapseSessionMeta_NoHeaderUnchanged(t *testing.T) {
	for _, in := range []string{
		"some assistant text\nmore text\n📂 not-a-header-this-is-body",
		"✅ Task Completed\njust a body line, no cwd",
		"",
	} {
		if got := CollapseSessionMeta(in); got != in {
			t.Errorf("non-header text should be unchanged:\n got=%q\n in=%q", got, in)
		}
	}
}
