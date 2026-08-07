package cmd

import (
	"strings"
	"testing"

	"github.com/Seraphli/tg-cli/cmd/helpers"
)

// TestBuildMailboxChunks_SingleShort verifies short text produces exactly one chunk
// with full header and body.
func TestBuildMailboxChunks_SingleShort(t *testing.T) {
	chunks := buildMailboxChunks("📥 Mail Received", "alice", "bob", "2026-04-11T10:00:00Z", "msg001", "Test Subject", "short body", "")
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	out := chunks[0]
	if !strings.Contains(out, "📥 Mail Received") {
		t.Error("first chunk missing firstLine")
	}
	if !strings.Contains(out, "<details><summary>📬 From: alice → bob</summary>") {
		t.Errorf("first chunk missing collapsible From→To summary: %q", out)
	}
	if !strings.Contains(out, "Subject: Test Subject") {
		t.Error("first chunk missing Subject line")
	}
	if !strings.Contains(out, "short body") {
		t.Error("first chunk missing body")
	}
}

// TestBuildMailboxChunks_RichHeaderStructure verifies improvements 1-3: the metadata header is a
// collapsible <details> block (summary From→To; body has the 🔖 mailbox hex ID and the archive-ID
// placeholder), the subject stays OUTSIDE the block, and separators are <hr/>.
func TestBuildMailboxChunks_RichHeaderStructure(t *testing.T) {
	out := buildMailboxChunks("", "alice", "bob", "2026-04-11T10:00:00Z", "1434f78654f5998d", "Hi", "body", "")[0]
	if !strings.Contains(out, "<details><summary>📬 From: alice → bob</summary>") {
		t.Errorf("missing details summary: %q", out)
	}
	if !strings.Contains(out, "🔖 1434f78654f5998d") {
		t.Errorf("missing 🔖 mailbox hex ID inside details: %q", out)
	}
	if !strings.Contains(out, helpers.ArchiveIDPlaceholder) {
		t.Errorf("missing archive-ID placeholder (filled at send time): %q", out)
	}
	if !strings.Contains(out, "<hr/>") {
		t.Errorf("missing <hr/> separator: %q", out)
	}
	// Subject must be OUTSIDE (after) the </details> block.
	if d, s := strings.Index(out, "</details>"), strings.Index(out, "Subject: Hi"); d < 0 || s < 0 || s < d {
		t.Errorf("Subject must be outside/after the details block: %q", out)
	}
	// The mailbox hex ID must be INSIDE the details block (before </details>).
	if h, d := strings.Index(out, "🔖 1434f78654f5998d"), strings.Index(out, "</details>"); h < 0 || d < 0 || h > d {
		t.Errorf("🔖 hex ID must be inside the details block: %q", out)
	}
}

// TestBuildMailboxChunks_Fix1BoldSubjectNoSep verifies Fix 1 (no separator between the collapsed
// <details> header and the Subject line — the block boundary is the divider) and Fix 2 (the Subject
// line is bold). Exactly one <hr/> remains, sitting AFTER the Subject as the subject→body separator.
func TestBuildMailboxChunks_Fix1BoldSubjectNoSep(t *testing.T) {
	out := buildMailboxChunks("", "alice", "bob", "2026-04-11T10:00:00Z", "abc123", "Hi", "body", "")[0]
	// Fix 2: the Subject line is bold.
	if !strings.Contains(out, "<b>Subject: Hi</b>") {
		t.Errorf("Subject line must be bold: %q", out)
	}
	d := strings.Index(out, "</details>")
	s := strings.Index(out, "<b>Subject:")
	if d < 0 || s < 0 || s < d {
		t.Fatalf("Subject must come after </details>: %q", out)
	}
	// Fix 1: no <hr/> between the details header and the Subject line.
	if strings.Contains(out[d:s], "<hr/>") {
		t.Errorf("no separator allowed between the details header and Subject: %q", out[d:s])
	}
	// Exactly one <hr/> remains, and it is after the Subject (the subject→body separator).
	if n := strings.Count(out, "<hr/>"); n != 1 {
		t.Errorf("expected exactly one <hr/> (subject→body sep), got %d: %q", n, out)
	}
	if hr := strings.Index(out, "<hr/>"); hr < 0 || hr < s {
		t.Errorf("the <hr/> separator must sit after the Subject line: %q", out)
	}
}

// TestBuildMailboxChunks_ArchiveIDInjected verifies the placeholder becomes 🆔 #<id> at send-time
// finalization (id>0), and is removed cleanly when there is no id (id==0 / archiving off).
func TestBuildMailboxChunks_ArchiveIDInjected(t *testing.T) {
	rich := buildMailboxChunks("", "alice", "bob", "2026-04-11T10:00:00Z", "abc123", "Hi", "body", "")[0]
	withID := helpers.FinalizeRichHTMLWithID(rich, 42)
	if !strings.Contains(withID, "🆔 #42") {
		t.Errorf("expected 🆔 #42 after finalize with id: %q", withID)
	}
	if strings.Contains(withID, helpers.ArchiveIDPlaceholder) {
		t.Errorf("placeholder must be replaced: %q", withID)
	}
	noID := helpers.FinalizeRichHTMLWithID(rich, 0)
	if strings.Contains(noID, helpers.ArchiveIDPlaceholder) || strings.Contains(noID, "🆔") {
		t.Errorf("id 0 must drop the placeholder and add no 🆔: %q", noID)
	}
}

// TestBuildMailboxChunksLegacy_NoRichTags verifies the legacy fallback uses flat text (no <details>,
// no <hr>, no placeholder — parse_mode=HTML supports none of these and the legacy body is sent verbatim).
func TestBuildMailboxChunksLegacy_NoRichTags(t *testing.T) {
	out := buildMailboxChunksLegacy("", "alice", "bob", "2026-04-11T10:00:00Z", "abc123", "Hi", "body", "")[0]
	if !strings.Contains(out, "📤 From: alice") {
		t.Errorf("legacy header should be flat text with 📤 From: %q", out)
	}
	if !strings.Contains(out, "🔖 abc123") {
		t.Errorf("legacy header should show the 🔖 mailbox hex ID: %q", out)
	}
	if !strings.Contains(out, "━━━━━━━━━━") {
		t.Errorf("legacy header should use ━ text separators: %q", out)
	}
	for _, bad := range []string{"<details>", "<hr/>", helpers.ArchiveIDPlaceholder} {
		if strings.Contains(out, bad) {
			t.Errorf("legacy chunk must not contain %q: %q", bad, out)
		}
	}
}

// TestBuildMailboxChunks_MarkdownRendering verifies markdown in subject and text
// is converted to Telegram HTML tags, and the status line is appended to the end
// of the (single) chunk (body bottom, not header).
func TestBuildMailboxChunks_MarkdownRendering(t *testing.T) {
	chunks := buildMailboxChunks("", "alice", "bob", "2026-04-11T10:00:00Z", "msg002", "Bold Subject", "**bold text** and *italic text* with `code`", "📫 Unread")
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	out := chunks[0]
	if !strings.Contains(out, "<b>bold text</b>") {
		t.Errorf("expected <b>bold text</b>, chunk: %s", out)
	}
	if !strings.Contains(out, "<i>italic text</i>") {
		t.Errorf("expected <i>italic text</i>, chunk: %s", out)
	}
	if !strings.Contains(out, "<code>code</code>") {
		t.Errorf("expected <code>code</code>, chunk: %s", out)
	}
	if !strings.HasSuffix(out, "📫 Unread") {
		t.Errorf("status line must be at end of chunk, got: %s", out)
	}
	// Status must come AFTER the body (rendered from markdown), not between header fields.
	bodyIdx := strings.Index(out, "<b>bold text</b>")
	statusIdx := strings.LastIndex(out, "📫 Unread")
	if bodyIdx == -1 || statusIdx == -1 || statusIdx < bodyIdx {
		t.Errorf("status must come after body, bodyIdx=%d statusIdx=%d", bodyIdx, statusIdx)
	}
	// Header block (From/To/Timestamp/ID/Subject separator) must NOT contain the status line.
	headerEnd := strings.Index(out, "Subject:")
	if headerEnd > 0 && strings.Contains(out[:headerEnd], "📫") {
		t.Errorf("status line leaked into header block, header: %s", out[:headerEnd])
	}
}

// TestBuildMailboxChunks_LongBodyMultiChunk verifies long body splits into multiple
// chunks, each covering a distinct portion of the text, with markers "📄 i/N".
// This guards against Bug 3A where chunks[1..] were silently dropped.
func TestBuildMailboxChunks_LongBodyMultiChunk(t *testing.T) {
	// Build a body with 5 uniquely-identifiable segments, each ~7500 ASCII chars.
	// Rich migration raised the mailbox body budget from mailboxBodyMaxRunes (3000) to
	// RichMaxRunes-500 (~29500), so the body must now exceed ~29500 runes to still exercise
	// multi-chunk splitting. 5×7500 = ~37500 runes → 2 chunks (kept ≤ 9 for single-digit markers).
	var segments []string
	for i := 0; i < 5; i++ {
		segments = append(segments, strings.Repeat("X", 7499)+"|SEG"+string(rune('A'+i))+"|")
	}
	longBody := strings.Join(segments, "\n\n")
	chunks := buildMailboxChunks("", "alice", "bob", "2026-04-11T10:00:00Z", "msg003", "Long Mail", longBody, "")
	if len(chunks) < 2 {
		t.Fatalf("expected multi-chunk, got %d chunks", len(chunks))
	}
	// Combined chunks must preserve every segment marker (no drop).
	joined := strings.Join(chunks, "\n")
	for i := 0; i < 5; i++ {
		marker := "|SEG" + string(rune('A'+i)) + "|"
		if !strings.Contains(joined, marker) {
			t.Errorf("segment marker %s missing from chunks — data loss!", marker)
		}
	}
	// First chunk must contain the header.
	if !strings.Contains(chunks[0], "📬 From: alice → bob") {
		t.Error("first chunk missing header")
	}
	// All chunks must contain "📄 i/N" marker.
	for i, c := range chunks {
		expected := "📄 " + string(rune('0'+i+1)) + "/" + string(rune('0'+len(chunks)))
		if !strings.Contains(c, expected) {
			t.Errorf("chunk %d missing marker %q", i, expected)
		}
	}
	// Continuation chunks (i > 0) must NOT contain full header.
	for i := 1; i < len(chunks); i++ {
		if strings.Contains(chunks[i], "📬 From:") {
			t.Errorf("continuation chunk %d unexpectedly contains header", i)
		}
	}
}

// TestBuildMailboxChunks_StatusInLastChunk verifies that when a long body splits
// into multiple chunks, the status line ("📫 Unread") is appended ONLY to the
// last chunk (at body bottom) and does NOT appear in any earlier chunks nor in
// the header block of the first chunk. This guarantees editTGReadReceipt can
// target just the last TG message to flip read/unread without touching earlier
// continuation messages.
func TestBuildMailboxChunks_StatusInLastChunk(t *testing.T) {
	// Build a long body (5 segments × ~7500 chars ≈ 37500 runes) that exceeds the rich mailbox
	// budget (RichMaxRunes-500 ≈ 29500) and so splits into >= 2 chunks.
	var segments []string
	for i := 0; i < 5; i++ {
		segments = append(segments, strings.Repeat("Y", 7499)+"|STATUS_TEST_SEG"+string(rune('A'+i))+"|")
	}
	longBody := strings.Join(segments, "\n\n")
	status := "📫 Unread"
	chunks := buildMailboxChunks("", "alice", "bob", "2026-04-11T10:00:00Z", "msg004", "Status Test", longBody, status)
	if len(chunks) < 2 {
		t.Fatalf("expected multi-chunk for long body, got %d", len(chunks))
	}
	last := chunks[len(chunks)-1]
	if !strings.HasSuffix(last, status) {
		tailStart := len(last) - 120
		if tailStart < 0 {
			tailStart = 0
		}
		t.Errorf("last chunk must end with status %q, last chunk tail: %q", status, last[tailStart:])
	}
	// Earlier chunks must NOT contain the status marker at all.
	for i := 0; i < len(chunks)-1; i++ {
		if strings.Contains(chunks[i], status) {
			t.Errorf("chunk %d (not last) unexpectedly contains status marker %q", i, status)
		}
	}
	// First chunk header block (before Subject separator) must not contain status — guards
	// against regression of the previous design where status lived inside the header.
	first := chunks[0]
	headerEnd := strings.Index(first, "Subject:")
	if headerEnd > 0 && strings.Contains(first[:headerEnd], status) {
		t.Errorf("first chunk header contains status — should only appear in last chunk body tail")
	}
}
