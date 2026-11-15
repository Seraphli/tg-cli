package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	tele "gopkg.in/telebot.v3"
)

// startRichPoller runs a RichReplyPoller against a fake getUpdates endpoint that answers its first request with
// result (a JSON array) and every later one with an empty result. It returns the delivered updates and a
// function that lists the request bodies received so far.
func startRichPoller(t *testing.T, result string) (chan tele.Update, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		n := len(bodies)
		mu.Unlock()
		res := "[]"
		if n == 1 {
			res = result
		} else {
			time.Sleep(20 * time.Millisecond)
		}
		w.Write([]byte(`{"ok":true,"result":` + res + `}`))
	}))
	bot, err := tele.NewBot(tele.Settings{URL: srv.URL, Token: "t", Offline: true, Client: srv.Client()})
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	dest := make(chan tele.Update, 8)
	stop := make(chan struct{})
	go (&RichReplyPoller{Timeout: 10 * time.Second}).Poll(bot, dest, stop)
	t.Cleanup(func() { close(stop); srv.Close() })
	return dest, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), bodies...)
	}
}

func nextUpdate(t *testing.T, dest chan tele.Update) tele.Update {
	t.Helper()
	select {
	case u := <-dest:
		return u
	case <-time.After(5 * time.Second):
		t.Fatal("no update delivered")
		return tele.Update{}
	}
}

// TestRichReply_GroupReachesSession is the boss's case end to end: a user replies in a forum topic to a rich
// stream message (the captured update with the pane, chat and topic of the test environment). The update goes
// through the poller and the registered handlers; the session pane receives the reply with the body of the
// replied message quoted (not its header), and no "No tmux session info" answer is sent. The poller asks for the
// update after it with the next getUpdates (offset = the update id + 1).
func TestRichReply_GroupReachesSession(t *testing.T) {
	e := newTopicRootEnv(t)
	const text = "richreply-1"
	const quote = "> R-c probe: please reply to this message"
	raw, err := os.ReadFile("testdata/rich_reply_update.json")
	if err != nil {
		t.Fatal(err)
	}
	var upd map[string]any
	if err := json.Unmarshal(raw, &upd); err != nil {
		t.Fatal(err)
	}
	msg := upd["message"].(map[string]any)
	msg["message_thread_id"], msg["is_topic_message"], msg["text"] = 12, true, text
	fixed, _ := json.Marshal(upd)
	result := strings.NewReplacer("-1001234567890", fmt.Sprint(forumChat.ID), "%999@/tmp/tmux-1000/default", e.pane).Replace(string(fixed))
	dest, requests := startRichPoller(t, "["+result+"]")
	e.deliver(nextUpdate(t, dest).Message, quote+"\n\n"+text)
	deadline := time.Now().Add(5 * time.Second)
	for len(requests()) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	var params map[string]string
	if reqs := requests(); len(reqs) < 2 || json.Unmarshal([]byte(reqs[1]), &params) != nil || params["offset"] != "745613306" {
		t.Errorf("second getUpdates = %v, want offset 745613306 (the update id + 1)", requests())
	}
	for _, r := range e.replies() {
		if strings.Contains(r, "No tmux session info") {
			t.Errorf("reply %q sent for a reply to a rich message", r)
		}
	}
	pane := e.paneText(t)
	if q, r := strings.Index(pane, quote), strings.Index(pane, text); q < 0 || r < q {
		t.Errorf("pane has the quote of the body at %d and the reply at %d, want both, the quote first:\n%s", q, r, pane)
	}
	if strings.Contains(pane, "📟") {
		t.Errorf("pane holds a header line of the replied message:\n%s", pane)
	}
	if want := "Injected reply to " + e.pane + " voice=false text=" + text; !strings.Contains(e.logText(t), want) {
		t.Errorf("log has no %q", want)
	}
}
