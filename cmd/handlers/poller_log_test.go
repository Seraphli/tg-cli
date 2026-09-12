package handlers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Seraphli/tg-cli/internal/logger"
	tele "gopkg.in/telebot.v3"
)

// finitePoller is a fake inner tele.Poller that pushes a fixed set of updates then blocks on stop, per
// the Poller contract ("must listen for stop constantly and close it as soon as it's done polling").
type finitePoller struct{ updates []tele.Update }

func (p *finitePoller) Poll(b *tele.Bot, dest chan tele.Update, stop chan struct{}) {
	for _, u := range p.updates {
		select {
		case dest <- u:
		case <-stop:
			return
		}
	}
	<-stop
}

// TestNewIncomingLogPoller_LogsBeforeDelivery covers T6 Group E#12: every update pumped through the
// wrapped poller gets exactly one INFO + one DEBUG "TG recv" log line, each update is still delivered
// downstream (the wrapper returns true unconditionally), and no nil-deref panics occur for an
// unhandled-type Audio message, a callback, or a bare non-message Update{} (Message/Chat/Sender all nil).
func TestNewIncomingLogPoller_LogsBeforeDelivery(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "poller.log")
	logger.Init(logPath, true) // debug=true so both INFO and DEBUG entries land in the file (writeLog is debug-gated).
	t.Cleanup(func() { logger.Init("", false) })

	updates := []tele.Update{
		// (a) an Audio message with no registered handler downstream — exactly the case this poller-level
		// log exists for, since telebot's bot.Use middleware silently skips unregistered update types.
		{Message: &tele.Message{ID: 1, Chat: &tele.Chat{ID: 10}, Sender: &tele.User{ID: 20}, Audio: &tele.Audio{File: tele.File{FileID: "a1"}}}},
		// (b) a callback update.
		{Callback: &tele.Callback{Sender: &tele.User{ID: 30}, Message: &tele.Message{ID: 2, Chat: &tele.Chat{ID: 11}}, Data: "somedata"}},
		// (c) a non-message update: Message/Chat/Sender all nil.
		{},
	}

	mw := NewIncomingLogPoller(&finitePoller{updates: updates})

	dest := make(chan tele.Update, len(updates))
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { mw.Poll(nil, dest, stop); close(done) }()

	var got []tele.Update
	for i := 0; i < len(updates); i++ {
		select {
		case u := <-dest:
			got = append(got, u)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for update %d to be delivered", i)
		}
	}
	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Poll to return after stop")
	}

	if len(got) != len(updates) {
		t.Fatalf("delivered %d updates, want %d (the wrapper must still deliver every update)", len(got), len(updates))
	}
	if got[0].Message == nil || got[0].Message.Audio == nil {
		t.Error("update (a) was not delivered intact (Audio message)")
	}
	if got[1].Callback == nil {
		t.Error("update (b) was not delivered intact (callback)")
	}
	if got[2].Message != nil || got[2].Callback != nil {
		t.Error("update (c) was not delivered intact (bare non-message update)")
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	log := string(data)

	if infoCount := strings.Count(log, "[INFO]"); infoCount != 3 {
		t.Errorf("INFO line count = %d, want 3 (one per update)", infoCount)
	}
	if debugCount := strings.Count(log, "[DEBUG]"); debugCount != 3 {
		t.Errorf("DEBUG line count = %d, want 3 (one per update)", debugCount)
	}
	if !strings.Contains(log, "TG recv audio: chat=10") {
		t.Errorf("missing the audio update's INFO line, log=%q", log)
	}
	if !strings.Contains(log, "TG recv callback: chat=11 sender=30 msg_id=2 data=somedata") {
		t.Errorf("missing the callback update's INFO line, log=%q", log)
	}
	if !strings.Contains(log, "TG recv other") {
		t.Errorf("missing the bare-update INFO line, log=%q", log)
	}
	if !strings.Contains(log, "TG recv raw:") {
		t.Errorf("missing raw JSON DEBUG lines, log=%q", log)
	}
}
