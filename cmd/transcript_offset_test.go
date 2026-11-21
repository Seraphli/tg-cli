package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Seraphli/tg-cli/cmd/helpers"
	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/cmd/types"
)

const (
	codexText = `{"type":"response_item","payload":{"role":"assistant","content":[{"type":"output_text","text":%q}]}}` + "\n"
	codexCall = `{"type":"response_item","payload":{"type":"function_call","name":"shell"}}` + "\n"
)

// bytesRead returns what this process has read through read(2) so far.
func bytesRead(t *testing.T) int64 {
	t.Helper()
	b, err := os.ReadFile("/proc/self/io")
	if err != nil {
		t.Fatal(err)
	}
	var n int64
	fmt.Sscanf(string(b), "rchar: %d", &n)
	return n
}

// rollout writes a transcript of mb MB of tool-output lines followed by the given assistant texts.
func rollout(t *testing.T, mb int, texts ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	out := strings.Repeat(`{"type":"response_item","payload":{"type":"function_call_output","output":"`+strings.Repeat("x", 1<<20)+`"}}`+"\n", mb)
	for _, s := range texts {
		out += fmt.Sprintf(codexText, s)
	}
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func appendTo(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err == nil {
		_, err = f.WriteString(s)
		f.Close()
	}
	if err != nil {
		t.Error(err)
	}
}

func offsetStore() *types.BotState {
	return &types.BotState{SessionCounts: stores.NewSessionCountStore()}
}

// A codex update reads the bytes appended since the last one, not the 40 MB before them, and sends each text once.
func TestCodexUpdateReadsOnlyAppendedBytes(t *testing.T) {
	path := rollout(t, 40, "Earlier reply.")
	bs := offsetStore()
	bs.SessionCounts.Offsets["s1"] = helpers.TranscriptSize(path) // what the codex UserPromptSubmit stores
	appendTo(t, path, fmt.Sprintf(codexText, "I will run the tests.")+codexCall)
	before := bytesRead(t)
	if got := processTranscriptUpdates(bs, "s1", path); got != "I will run the tests." {
		t.Fatalf("update = %q", got)
	}
	if n := bytesRead(t) - before; n > 2<<20 {
		t.Errorf("an update read %d bytes of a 40 MB transcript", n)
	}
	appendTo(t, path, fmt.Sprintf(codexText, "All tests pass.")+codexCall)
	if got := processTranscriptUpdates(bs, "s1", path); got != "All tests pass." {
		t.Errorf("second update = %q", got)
	}
}

// The hook arrives while codex is still writing the next reply line: the finished line is sent on the next update.
func TestCodexUpdateKeepsAHalfWrittenLine(t *testing.T) {
	path := rollout(t, 0)
	bs := offsetStore()
	bs.SessionCounts.Offsets["s1"] = 0
	line := fmt.Sprintf(codexText, "Run finished: 12 passed.")
	appendTo(t, path, fmt.Sprintf(codexText, "Starting the run.")+line[:len(line)/2])
	if got := processTranscriptUpdates(bs, "s1", path); got != "Starting the run." {
		t.Fatalf("first update = %q", got)
	}
	appendTo(t, path, line[len(line)/2:]+codexCall)
	if got := processTranscriptUpdates(bs, "s1", path); got != "Run finished: 12 passed." {
		t.Errorf("second update = %q", got)
	}
}

// After a bot restart a session has no stored offset: history is not sent, and a question still carries the text before it.
func TestUnknownSessionStart(t *testing.T) {
	t.Run("update", func(t *testing.T) {
		path := rollout(t, 40, "old one", "old two")
		go func() {
			time.Sleep(200 * time.Millisecond)
			appendTo(t, path, fmt.Sprintf(codexText, "New text.")+codexCall)
		}()
		before := bytesRead(t)
		if got := processTranscriptUpdates(offsetStore(), "s2", path); got != "New text." {
			t.Errorf("update = %q", got)
		}
		if n := bytesRead(t) - before; n > 2<<20 {
			t.Errorf("starting the session read %d bytes of a 40 MB transcript", n)
		}
	})
	t.Run("question", func(t *testing.T) {
		if got := processTranscriptUpdates(offsetStore(), "s2", rollout(t, 0, "old one", "old two"), true); got != "old two" {
			t.Errorf("question update = %q", got)
		}
	})
}

// The Stop and UserPromptSubmit hooks of a cc session on a 40 MB transcript read none of it; the Stop message still goes out.
func TestCCHooksReadNoTranscript(t *testing.T) {
	rig := newHookRig(t, "cc")
	path := rollout(t, 40, "Done: 3 files changed.")
	before := bytesRead(t)
	rig.post("UserPromptSubmit", map[string]interface{}{"transcript_path": path, "prompt": "go"})
	rig.post("Stop", map[string]interface{}{"transcript_path": path, "project": "proj", "last_assistant_message": "Done: 3 files changed."})
	waitUntil(t, 5*time.Second, func() bool { return rig.tg.Count("sendRichMessage") >= 1 })
	if n := bytesRead(t) - before; n > 4<<20 {
		t.Errorf("the hooks read %d bytes of a 40 MB transcript", n)
	}
}

// A codex Stop or UserPromptSubmit moves the offset past what the turn already handled, so the next update does not repeat it.
func TestCodexHooksMoveTheOffset(t *testing.T) {
	for _, event := range []string{"Stop", "UserPromptSubmit"} {
		t.Run(event, func(t *testing.T) {
			rig := newHookRig(t, "codex")
			path := rollout(t, 0, "Earlier text.")
			rig.post(event, map[string]interface{}{"transcript_path": path, "project": "proj", "last_assistant_message": "Earlier text."})
			lock := rig.bs.SessionCounts.GetLock(rigSession)
			waitUntil(t, 3*time.Second, func() bool {
				lock.Lock()
				defer lock.Unlock()
				_, ok := rig.bs.SessionCounts.Offsets[rigSession]
				return ok
			})
			appendTo(t, path, fmt.Sprintf(codexText, "Next task started.")+codexCall)
			if got := processTranscriptUpdates(rig.bs, rigSession, path); got != "Next task started." {
				t.Errorf("update after %s = %q", event, got)
			}
		})
	}
}
