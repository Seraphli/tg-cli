package cmd

import (
	"strings"
	"testing"
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
	if !strings.Contains(out, "📤 From: alice") {
		t.Error("first chunk missing From header")
	}
	if !strings.Contains(out, "Subject: Test Subject") {
		t.Error("first chunk missing Subject line")
	}
	if !strings.Contains(out, "short body") {
		t.Error("first chunk missing body")
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
	// Build a body with 5 uniquely-identifiable segments, each ~1000 ASCII chars.
	var segments []string
	for i := 0; i < 5; i++ {
		segments = append(segments, strings.Repeat("X", 999)+"|SEG"+string(rune('A'+i))+"|")
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
	if !strings.Contains(chunks[0], "📤 From: alice") {
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
		if strings.Contains(chunks[i], "📤 From:") {
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
	// Build a long body (5 segments × ~1000 chars) that will split into >= 2 chunks.
	var segments []string
	for i := 0; i < 5; i++ {
		segments = append(segments, strings.Repeat("Y", 999)+"|STATUS_TEST_SEG"+string(rune('A'+i))+"|")
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
