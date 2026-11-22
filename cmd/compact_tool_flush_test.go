package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/tgfake"
)

// compactRig is newHookRig with compact tool notifications on and a transcript that holds 20 Read tool calls;
// read(n) posts the PreToolUse of call n (it reads /tmp/card-NN.txt).
func compactRig(t *testing.T, backend string) (*hookRig, func(n int)) {
	t.Helper()
	rig := newHookRig(t, backend)
	rig.bs.ReactionTracker = stores.NewReactionTrackerStore()
	if err := os.WriteFile(config.GetConfigPath(), []byte(`{"toolNotifyCompact":true,"toolNotifyList":["Read"]}`), 0644); err != nil {
		t.Fatal(err)
	}
	var transcript strings.Builder
	for i := 1; i <= 20; i++ {
		line, _ := json.Marshal(map[string]interface{}{"type": "assistant", "message": map[string]interface{}{"id": fmt.Sprintf("msg-%d", i),
			"content": []map[string]interface{}{{"type": "tool_use", "id": fmt.Sprintf("toolu-%d", i), "name": "Read", "input": map[string]string{"file_path": fmt.Sprintf("/tmp/card-%02d.txt", i)}}}}})
		transcript.Write(line)
		transcript.WriteByte('\n')
	}
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(path, []byte(transcript.String()), 0644); err != nil {
		t.Fatal(err)
	}
	return rig, func(n int) {
		rig.post("PreToolUse", map[string]interface{}{"prompt_id": "P1", "tool_name": "Read", "tool_use_id": fmt.Sprintf("toolu-%d", n),
			"transcript_path": path, "tool_input": map[string]string{"file_path": fmt.Sprintf("/tmp/card-%02d.txt", n)}})
	}
}

// cardsIn counts the tool lines (card-01.txt to card-18.txt) a message body carries.
func cardsIn(body string) (n int) {
	for i := 1; i <= 18; i++ {
		if strings.Contains(body, fmt.Sprintf("card-%02d.txt", i)) {
			n++
		}
	}
	return n
}

// TestCompactToolBurst_OneEditPerWindowFlushedBeforeSentence: the 2026-11-20 burst, 18 tool calls 0.8 s apart,
// then the agent's sentence, with Telegram answering the second edit with a flood wait of 2 s. The compact tool
// message is edited once per 5 s window (not once per line), the pending lines are edited in before the sentence
// goes out below, the edit after the flood wait carries the lines that arrived meanwhile, and the Message FIFO
// is never parked in the flood sleep.
func TestCompactToolBurst_OneEditPerWindowFlushedBeforeSentence(t *testing.T) {
	rig, read := compactRig(t, "cc")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go startStreamLoop(ctx, rig.bs)
	const sentence = "The eighteen cards are open."
	type event struct {
		kind string // "edit", "flood" or "sentence"
		at   time.Time
		body string
	}
	var mu sync.Mutex
	var events []event
	record := func(kind string, p map[string]interface{}) {
		body, _ := json.Marshal(p)
		events = append(events, event{kind, time.Now(), string(body)})
	}
	ok := tgfake.Reply{Result: map[string]interface{}{"message_id": 9001, "chat": map[string]interface{}{"id": 1}}}
	rig.tg.On("editMessageText", func(p map[string]interface{}) tgfake.Reply {
		mu.Lock()
		defer mu.Unlock()
		if edits := len(events); edits == 1 { // the second edit request (the first event is the first edit)
			record("flood", p)
			return tgfake.Reply{RetryAfter: 2}
		}
		record("edit", p)
		return ok
	})
	rig.tg.On("sendRichMessage", func(p map[string]interface{}) tgfake.Reply {
		mu.Lock()
		defer mu.Unlock()
		if body, _ := json.Marshal(p); strings.Contains(string(body), sentence) {
			record("sentence", p)
		}
		return ok
	})
	probed := false
	for n := 1; n <= 18; n++ {
		read(n)
		time.Sleep(800 * time.Millisecond)
		mu.Lock()
		flooding := len(events) > 1 && events[1].kind == "flood" && time.Since(events[1].at) < 2*time.Second
		mu.Unlock()
		if flooding && !probed { // a probe on the Message FIFO during the flood wait must not wait for it
			probed = true
			start := time.Now()
			rig.bs.MessageQueue.Dispatch(rigSession, "probe", func() error { return nil })
			if d := time.Since(start); d > 200*time.Millisecond {
				t.Errorf("the Message FIFO was parked for %v during the flood wait", d)
			}
		}
	}
	time.Sleep(400 * time.Millisecond)
	rig.post("MessageDisplay", map[string]interface{}{"message_id": "m-sentence", "turn_id": "T1", "prompt_id": "P1", "index": 0, "final": true, "delta": sentence})
	waitUntil(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return events[len(events)-1].kind == "sentence"
	})
	mu.Lock()
	defer mu.Unlock()
	edits := 0
	for _, e := range events {
		if e.kind == "edit" {
			edits++
		}
	}
	if edits > 5 {
		t.Errorf("%d edits for 18 lines, want at most 5", edits)
	}
	last := events[len(events)-2] // the event just before the sentence
	if last.kind != "edit" || cardsIn(last.body) != 18 {
		t.Errorf("before the sentence went out the last event is %s with %d of 18 lines, want an edit with all 18", last.kind, cardsIn(last.body))
	}
	if !probed {
		t.Error("no flood wait was observed")
	}
	for i, e := range events {
		if e.kind != "flood" {
			continue
		}
		next := events[i+1]
		if next.kind != "edit" || next.body == e.body || cardsIn(next.body) <= cardsIn(e.body) || next.at.Sub(e.at) < 1900*time.Millisecond {
			t.Errorf("after the flood wait the next event is %s %v later with %d lines (the flooded edit had %d), want a newer edit after the 2 s wait", next.kind, next.at.Sub(e.at), cardsIn(next.body), cardsIn(e.body))
		}
	}
}

// TestCompactTool_UserPromptSubmitClosesOpenMessage: a user prompt arrives while a compact tool message is open
// and the agent goes on with a tool call without writing text first. The pending line of the old message is
// edited in and the next tool line is a NEW message at the bottom, not an edit of the message above.
func TestCompactTool_UserPromptSubmitClosesOpenMessage(t *testing.T) {
	rig, read := compactRig(t, "cc")
	read(1) // sends message M1
	time.Sleep(300 * time.Millisecond)
	read(2) // pending in M1's window
	rig.post("UserPromptSubmit", map[string]interface{}{"prompt_id": "P1", "prompt": "go on"})
	read(3)
	time.Sleep(time.Second)
	var methods, bodies []string
	for _, c := range rig.tg.Calls() {
		if body, _ := json.Marshal(c.Params); c.Method == "sendRichMessage" || c.Method == "editMessageText" {
			methods, bodies = append(methods, c.Method), append(bodies, string(body))
		}
	}
	if strings.Join(methods, " ") != "sendRichMessage editMessageText sendRichMessage" {
		t.Fatalf("Telegram saw %v, want send M1, edit M1, send M2", methods)
	}
	if !strings.Contains(bodies[0], "card-01.txt") || !strings.Contains(bodies[1], "card-02.txt") || strings.Contains(bodies[1], "card-03.txt") ||
		!strings.Contains(bodies[2], "card-03.txt") || strings.Contains(bodies[2], "card-01.txt") {
		t.Fatalf("wrong lines: M1 %q, edit %q, M2 %q", bodies[0], bodies[1], bodies[2])
	}
}

// TestCompactTool_PendingEditBeforeMessageBelow: an agent makes tool calls, the second line is still inside the 5 s
// window of the compact message, and then a message goes out below: a codex agent's reply, a question a cc agent
// asks the boss, or the Session Started notice after a cc compaction (which also ends a held state). The pending
// line is edited into the message above BEFORE the message below is sent.
func TestCompactTool_PendingEditBeforeMessageBelow(t *testing.T) {
	const question = `{"session_id":"sess-late","tmux_target":"%900","backend":"cc","tool_name":"AskUserQuestion","tool_input":{"questions":[{"header":"Card","question":"Which card first?","multiSelect":false,"options":[{"label":"One","description":"card one"},{"label":"Two","description":"card two"}]}]}}`
	cases := []struct {
		name, backend, below string // below: text of the message sent below
		trigger              func(t *testing.T, rig *hookRig)
	}{
		{"reply", "codex", "Both cards are open.", func(t *testing.T, rig *hookRig) {
			rig.post("Stop", map[string]interface{}{"project": "proj", "last_assistant_message": "Both cards are open."})
		}},
		{"question", "cc", "Which card first?", func(t *testing.T, rig *hookRig) {
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			req, _ := http.NewRequestWithContext(ctx, "POST", rig.url+"/pending/connect?uuid=q1", strings.NewReader(question))
			go http.DefaultClient.Do(req)
		}},
		{"session start", "cc", "Session Started", func(t *testing.T, rig *hookRig) {
			// a /compact was the last item of a Force flush and its turn was interrupted
			rig.bs.InjectQueue.SetHeld("%900", 0)
			rig.post("SessionStart", map[string]interface{}{"source": "compact"}) // Claude Code ran the queued /compact right after the Esc; the compaction ended
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rig, read := compactRig(t, c.backend)
			read(1) // sends message M1
			time.Sleep(300 * time.Millisecond)
			read(2) // pending in M1's window
			c.trigger(t, rig)
			sent := func() (seen []string) {
				for _, c2 := range rig.tg.Calls() {
					body, _ := json.Marshal(c2.Params)
					switch {
					case c2.Method == "editMessageText" && strings.Contains(string(body), "card-02.txt"):
						seen = append(seen, "edit with line 2")
					case strings.Contains(string(body), c.below):
						seen = append(seen, "message below")
					}
				}
				return seen
			}
			waitUntil(t, 8*time.Second, func() bool { return slices.Contains(sent(), "message below") }) // a cc question waits up to 3 s for a new text bubble first
			time.Sleep(300 * time.Millisecond)
			if got := strings.Join(sent(), ", "); got != "edit with line 2, message below" {
				t.Fatalf("Telegram saw %q, want the edit with line 2 before the message below", got)
			}
			// only the session start case sets the held mark; the SessionStart handler clears it before it dispatches the notice
			if rig.bs.InjectQueue.Held("%900") {
				t.Fatal("the target is still held after the compaction ended")
			}
		})
	}
}
