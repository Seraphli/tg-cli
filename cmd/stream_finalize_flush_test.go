package cmd

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/cmd/types"
	tele "gopkg.in/telebot.v3"
)

// D4(a) end-to-end: when Stop installs the authoritative last_assistant_message onto a PARTIAL stream
// entry, the Stop flush must send the COMPLETE text (via the real render+send path), never the partial
// fragment — this is the Stop/MD truncation fix through a fake Bot API transport.
func TestStopFlush_RendersAuthoritativeFullText(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":555,"chat":{"id":1}}}`)
	}))
	defer srv.Close()

	bot, err := tele.NewBot(tele.Settings{Token: "test", URL: srv.URL, Offline: true, Client: srv.Client()})
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	// The dual-FIFO stop flush enqueues its render op onto the Message FIFO and touches MsgIDMap on the
	// op path, so both must be initialized or flushSession nil-panics (S3/S14 setup fix).
	bs := &types.BotState{Bot: bot, Streams: stores.NewStreamStore(), MessageQueue: stores.NewSessionEventStore(), MsgIDMap: stores.NewMsgIDMap()}
	sid := "sess-trunc"

	// A partial MD stream: one delta, NOT final (incomplete) — the truncation scenario.
	bs.Streams.AppendDelta(sid, stores.StreamMeta{MessageID: "m1", ChatID: 1}, 0, "partial fragment incomplete", false)
	// Stop arrives with the complete authoritative text.
	if got := bs.Streams.FinalizeLastWithText(sid, "the complete authoritative assistant reply"); got != stores.FinalizeExisting {
		t.Fatalf("FinalizeLastWithText = %v, want FinalizeExisting", got)
	}

	streamFlush(bs, sid, true)

	joined := strings.Join(bodies, "\n")
	if !strings.Contains(joined, "complete authoritative assistant reply") {
		t.Fatalf("Stop flush did not send the full authoritative text; sent=%q", joined)
	}
	if strings.Contains(joined, "partial fragment incomplete") {
		t.Errorf("Stop flush sent the PARTIAL text (truncation bug not fixed); sent=%q", joined)
	}
}

// newFakeStopBot returns a fake Bot API transport that records every send/edit request body, plus a live
// telebot handle wired to it. Used to count the terminal-outcome messages the Stop path produces.
func newFakeStopBot(t *testing.T, bodies *[]string) (*tele.Bot, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*bodies = append(*bodies, string(b))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":556,"chat":{"id":1}}}`)
	}))
	bot, err := tele.NewBot(tele.Settings{Token: "test", URL: srv.URL, Offline: true, Client: srv.Client()})
	if err != nil {
		srv.Close()
		t.Fatalf("NewBot: %v", err)
	}
	return bot, srv.Close
}

// runStopTerminal drives the S10 Stop terminal state machine (register.go PreToolUse/Stop) at the store/op
// level: install the authoritative body, then either sync-Dispatch a msg:stop-direct send (FinalizeNoEntry +
// non-empty body) or run the single SYNC stop render op — every branch ends in MarkStopped. This mirrors the
// register.go Stop CC branch WITHOUT the full interactive hook pipeline (the drain/wait/grace is gone in S10).
func runStopTerminal(bs *BotState, sessionID, body string) {
	if bs.Streams.FinalizeLastWithText(sessionID, body) == stores.FinalizeNoEntry && body != "" {
		bs.MessageQueue.Dispatch(sessionID, "msg:stop-direct", func() error {
			sendEventNotification(bs, &tele.Chat{ID: 1}, "1", sessionID, "Stop", "", "", "", body, "", "", 0)
			return nil
		})
		bs.Streams.MarkStopped(sessionID)
		return
	}
	streamFlush(bs, sessionID, true)
}

// S14h Stop terminal (S10, no-grace): the OLD stopMDGrace/AwaitEntryOrStop late-MD wait was DELETED, so this
// asserts the new terminal outcomes directly — (1) FinalizeNoEntry + non-empty body sends EXACTLY ONE
// msg:stop-direct message and ends MarkStopped; (2) a no-text Stop sends nothing and still MarkStopped; (3) an
// INCOMPLETE last entry is force-finalized + rendered ONCE and ends MarkStopped.
func TestStopTerminal_NoGrace(t *testing.T) {
	newBS := func(bot *tele.Bot) *BotState {
		return &types.BotState{Bot: bot, Streams: stores.NewStreamStore(),
			MessageQueue: stores.NewSessionEventStore(), MsgIDMap: stores.NewMsgIDMap(),
			SessionState: stores.NewSessionStateStore(t.TempDir())}
	}
	// isStopped reads the session's terminal Stopped flag under DataMu (no public accessor exists; the field
	// is exported so the cmd package can read it — this asserts the S10 "every branch ends MarkStopped" guarantee).
	isStopped := func(bs *BotState, sid string) bool {
		ss := bs.Streams.Session(sid)
		ss.DataMu.Lock()
		defer ss.DataMu.Unlock()
		return ss.Stopped
	}

	t.Run("NoEntry+body: exactly one direct send + MarkStopped", func(t *testing.T) {
		var bodies []string
		bot, closeFn := newFakeStopBot(t, &bodies)
		defer closeFn()
		bs := newBS(bot)
		sid := "sess-direct"
		body := "the complete authoritative reply after the tool"
		// No stream entry exists → FinalizeNoEntry; a non-empty body must direct-send ONCE.
		if got := bs.Streams.FinalizeLastWithText(sid, body); got != stores.FinalizeNoEntry {
			t.Fatalf("FinalizeLastWithText = %v, want FinalizeNoEntry", got)
		}
		runStopTerminal(bs, sid, body)
		if len(bodies) != 1 {
			t.Fatalf("expected EXACTLY ONE direct-send message, got %d: %q", len(bodies), bodies)
		}
		if !strings.Contains(bodies[0], "complete authoritative reply after the tool") {
			t.Errorf("direct-send must carry the full authoritative body; got %q", bodies[0])
		}
		if !isStopped(bs, sid) {
			t.Fatal("Stop terminal must MarkStopped after the direct send")
		}
	})

	t.Run("no-text Stop: nothing sent + MarkStopped", func(t *testing.T) {
		var bodies []string
		bot, closeFn := newFakeStopBot(t, &bodies)
		defer closeFn()
		bs := newBS(bot)
		sid := "sess-notext"
		// No entry and an EMPTY body → FinalizeSkipped, no direct send, still MarkStopped via the render path.
		runStopTerminal(bs, sid, "")
		if len(bodies) != 0 {
			t.Fatalf("no-text Stop must send NOTHING, got %d: %q", len(bodies), bodies)
		}
		if !isStopped(bs, sid) {
			t.Fatal("no-text Stop must still MarkStopped")
		}
	})

	t.Run("incomplete last entry: force-finalized + rendered once + MarkStopped", func(t *testing.T) {
		var bodies []string
		bot, closeFn := newFakeStopBot(t, &bodies)
		defer closeFn()
		bs := newBS(bot)
		sid := "sess-incomplete"
		// A partial (non-final) MD stream — the incomplete last entry. Stop arrives with NO authoritative body,
		// so FinalizeLastWithText is skipped and the force=true stop render must finalize+render the partial ONCE.
		bs.Streams.AppendDelta(sid, stores.StreamMeta{MessageID: "m1", ChatID: 1}, 0, "partial not final yet", false)
		runStopTerminal(bs, sid, "")
		if len(bodies) != 1 {
			t.Fatalf("incomplete last entry must be rendered EXACTLY ONCE, got %d: %q", len(bodies), bodies)
		}
		if !strings.Contains(bodies[0], "partial not final yet") {
			t.Errorf("forced stop render must send the partial text; got %q", bodies[0])
		}
		if !isStopped(bs, sid) {
			t.Fatal("incomplete Stop must MarkStopped after the forced render")
		}
	})
}
