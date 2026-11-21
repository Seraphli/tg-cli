package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Seraphli/tg-cli/cmd/stores"
)

// TestUserPromptSubmitKeepsOwnTurnText: the 2026-11-21 06:33 upgrade. The new bot received MessageDisplay and
// PreToolUse of a turn before that turn's own UserPromptSubmit (the hook processes retried while the bot was
// down). The UserPromptSubmit must not wipe the unsent text of its own prompt: the PreToolUse that waits for that
// text must return, so the session's hook queue keeps moving, and the text must be sent once.
func TestUserPromptSubmitKeepsOwnTurnText(t *testing.T) {
	const text = "Mail delivered and the receiver hung again"
	line := func(block map[string]interface{}) string {
		b, _ := json.Marshal(map[string]interface{}{"type": "assistant", "message": map[string]interface{}{"id": "msg-1", "content": []map[string]interface{}{block}}})
		return string(b) + "\n"
	}
	transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
	lines := line(map[string]interface{}{"type": "text", "text": text}) +
		line(map[string]interface{}{"type": "tool_use", "id": "toolu-1", "name": "Bash", "input": map[string]string{"command": "true"}})
	if err := os.WriteFile(transcript, []byte(lines), 0644); err != nil {
		t.Fatal(err)
	}
	rig := newHookRig(t, "cc")
	rig.bs.ReactionTracker = stores.NewReactionTrackerStore()
	rig.post("MessageDisplay", map[string]interface{}{"message_id": "m1", "turn_id": "T1", "prompt_id": "P1", "index": 0, "final": true, "delta": text})
	rig.post("UserPromptSubmit", map[string]interface{}{"prompt_id": "P1", "prompt": "a background job finished"})
	rig.post("PreToolUse", map[string]interface{}{"prompt_id": "P1", "tool_name": "Bash", "tool_use_id": "toolu-1", "transcript_path": transcript})
	ran := make(chan struct{})
	rig.bs.SessionEvents.DispatchAsync(rigSession, "probe", func() error { close(ran); return nil })
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("the session's hook queue is parked: PreToolUse waits for text the UserPromptSubmit wiped")
	}
	sends := func() (n int) {
		for _, c := range rig.tg.Calls() {
			if body, _ := json.Marshal(c.Params); strings.HasPrefix(c.Method, "send") && strings.Contains(string(body), text) {
				n++
			}
		}
		return n
	}
	waitUntil(t, 3*time.Second, func() bool { return sends() >= 1 })
	time.Sleep(time.Second)
	if n := sends(); n != 1 {
		t.Fatalf("the text was sent %d times, want 1", n)
	}
}
