package handlers

import (
	"testing"

	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/cmd/types"
	tele "gopkg.in/telebot.v3"
)

// TestExtractReplyTarget_TextPath: a plain/legacy notification carries a 📟 line, so the target is
// parsed straight from the message text (no Pages lookup needed).
func TestExtractReplyTarget_TextPath(t *testing.T) {
	bs := &types.BotState{Pages: stores.NewPageCacheStore()}
	replyTo := &tele.Message{ID: 100, Text: "🔴 Idle\n📟 %5\n📊 Context"}
	target, err := extractReplyTarget(bs, replyTo)
	if err != nil {
		t.Fatalf("expected target from text, got err: %v", err)
	}
	if target == nil || target.PaneID != "%5" {
		t.Fatalf("expected PaneID %%5, got %+v", target)
	}
}

// TestExtractReplyTarget_PagesFallback: a rich Bot API 10.1 notification has an EMPTY .Text (its
// content lives in a rich_message field telebot does not model). The target must be recovered from the
// Pages msg_id -> target record written by recordReplyTarget when the notification was sent. This is
// the core of Fix 8.
func TestExtractReplyTarget_PagesFallback(t *testing.T) {
	bs := &types.BotState{Pages: stores.NewPageCacheStore()}
	bs.Pages.Store(200, "sess-1", &stores.PageEntry{TmuxTarget: "%7@/tmp/tmux-1000/default"})
	replyTo := &tele.Message{ID: 200, Text: ""}
	target, err := extractReplyTarget(bs, replyTo)
	if err != nil {
		t.Fatalf("expected Pages fallback to resolve target, got err: %v", err)
	}
	if target == nil || target.PaneID != "%7" || target.Socket != "/tmp/tmux-1000/default" {
		t.Fatalf("expected %%7@/tmp/tmux-1000/default, got %+v", target)
	}
}

// TestExtractReplyTarget_EmptyTextNoPages: empty text and no Pages record → no target (error).
func TestExtractReplyTarget_EmptyTextNoPages(t *testing.T) {
	bs := &types.BotState{Pages: stores.NewPageCacheStore()}
	replyTo := &tele.Message{ID: 300, Text: ""}
	if _, err := extractReplyTarget(bs, replyTo); err == nil {
		t.Fatal("expected error when text is empty and no Pages record exists")
	}
}

// TestExtractReplyTarget_TextWinsOverPages: when the text carries a 📟 line, it takes precedence over
// any Pages record (text is the authoritative content of a legacy notification).
func TestExtractReplyTarget_TextWinsOverPages(t *testing.T) {
	bs := &types.BotState{Pages: stores.NewPageCacheStore()}
	bs.Pages.Store(400, "sess-1", &stores.PageEntry{TmuxTarget: "%9"})
	replyTo := &tele.Message{ID: 400, Text: "🔴 Idle\n📟 %5"}
	target, err := extractReplyTarget(bs, replyTo)
	if err != nil || target == nil {
		t.Fatalf("expected target, got err: %v", err)
	}
	if target.PaneID != "%5" {
		t.Fatalf("text 📟 line should win over Pages: expected %%5, got %s", target.PaneID)
	}
}

// TestExtractReplyTarget_PagesEntryEmptyTarget: a Pages record with an empty TmuxTarget is not a
// valid resolution (guards against recordReplyTarget having stored a blank).
func TestExtractReplyTarget_PagesEntryEmptyTarget(t *testing.T) {
	bs := &types.BotState{Pages: stores.NewPageCacheStore()}
	bs.Pages.Store(500, "sess-1", &stores.PageEntry{TmuxTarget: ""})
	replyTo := &tele.Message{ID: 500, Text: ""}
	if _, err := extractReplyTarget(bs, replyTo); err == nil {
		t.Fatal("expected error when Pages entry has an empty TmuxTarget")
	}
}

// TestExtractReplyTarget_NilReply: a nil reply message yields an error.
func TestExtractReplyTarget_NilReply(t *testing.T) {
	bs := &types.BotState{Pages: stores.NewPageCacheStore()}
	if _, err := extractReplyTarget(bs, nil); err == nil {
		t.Fatal("expected error for nil reply message")
	}
}
