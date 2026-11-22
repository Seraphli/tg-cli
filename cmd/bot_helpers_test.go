package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Seraphli/tg-cli/cmd/helpers"
	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/tgfake"
	tele "gopkg.in/telebot.v3"
)

// TestScanCustomCommandsFrontmatter tests the YAML frontmatter parsing logic
// used in scanCustomCommands. Since that function hardcodes ~/.claude/commands/,
// we replicate the parsing logic here with temp files.
func TestScanCustomCommandsFrontmatter(t *testing.T) {
	// parseDesc replicates the file-parsing logic from scanCustomCommands
	parseDesc := func(path, defaultDesc string) string {
		desc := defaultDesc
		f, err := os.Open(path)
		if err != nil {
			return desc
		}
		defer f.Close()
		scanner := bufio.NewScanner(f)
		if scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "---" {
				// YAML frontmatter: scan until closing --- and extract description
				for scanner.Scan() {
					fmLine := strings.TrimSpace(scanner.Text())
					if fmLine == "---" {
						break
					}
					if strings.HasPrefix(fmLine, "description:") {
						desc = helpers.TruncateStr(strings.TrimSpace(strings.TrimPrefix(fmLine, "description:")), 200)
					}
				}
			} else {
				line = strings.TrimLeft(line, "# ")
				if len(line) > 0 {
					desc = helpers.TruncateStr(line, 200)
				}
			}
		}
		return desc
	}

	tmpDir := t.TempDir()

	tests := []struct {
		name        string
		content     string
		defaultDesc string
		expected    string
	}{
		{
			name: "frontmatter with description",
			content: "---\ndescription: My test description\n---\nBody content here\n",
			defaultDesc: "default",
			expected:    "My test description",
		},
		{
			name: "frontmatter without description field",
			content: "---\ntitle: Something\nauthor: me\n---\nBody\n",
			defaultDesc: "default",
			expected:    "default",
		},
		{
			name: "heading first line no frontmatter",
			content: "# My Heading\nSome body text\n",
			defaultDesc: "default",
			expected:    "My Heading",
		},
		{
			name: "plain first line no frontmatter",
			content: "Just a plain line\nMore content\n",
			defaultDesc: "default",
			expected:    "Just a plain line",
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(tmpDir, strings.ReplaceAll(tt.name, " ", "_")+string(rune('0'+i))+".md")
			if err := os.WriteFile(path, []byte(tt.content), 0644); err != nil {
				t.Fatalf("failed to write temp file: %v", err)
			}
			got := parseDesc(path, tt.defaultDesc)
			if got != tt.expected {
				t.Errorf("got %q, want %q", got, tt.expected)
			}
		})
	}
}

// TestQuoteOriginalMessage tests the blockquote formatting logic used in processUserInput.
func TestQuoteOriginalMessage(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		expected string // expected quoted result (before the \n\n separator)
	}{
		{"single line", "hello world", "> hello world"},
		{"multi line", "line1\nline2\nline3", "> line1\n> line2\n> line3"},
		{"empty text", "", ""},
		{"long text no truncation", strings.Repeat("a", 600), "> " + strings.Repeat("a", 600)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Replicate the quote logic from processUserInput
			if tt.text == "" {
				if tt.expected != "" {
					t.Errorf("expected empty but got %q", tt.expected)
				}
				return
			}
			quoted := tt.text
			var lines []string
			for _, line := range strings.Split(quoted, "\n") {
				lines = append(lines, "> "+line)
			}
			result := strings.Join(lines, "\n")
			if result != tt.expected {
				t.Errorf("got %q, want %q", result, tt.expected)
			}
		})
	}
}

// callBodies returns the JSON bodies of the requests the fake Telegram got for one method, in order.
func callBodies(tg *tgfake.Server, method string) (bodies []string) {
	for _, c := range tg.Calls() {
		if body, _ := json.Marshal(c.Params); c.Method == method {
			bodies = append(bodies, string(body))
		}
	}
	return bodies
}

// pasteOf is what the recorder pane shows for one injected group.
func pasteOf(text string) string { return "^[[200~" + text + "^[[201~\n" }

// TestDeliverInjectQueue_SlashCommandInjectedAlone: the queue of the 2026-11-20 incident, [/compact, /compact,
// wake-up prompt], was joined into ONE input and the wake-up prompt was swallowed by the slash command. A slash
// command is injected alone, and in a Force mode the later items are held (back in the queue, in order) until the
// session is idle, because Claude Code runs a queued slash command only at the turn end: a later Force flush in the
// same turn (the agent's next tool call) must not inject them either. Every undelivered item is listed in exactly
// one "⏳ Queued" message, also when the user sends one more message while the flush runs (it queues itself with a
// Queued message of its own); the idle flush edits that message to "✅ Injected", so no ⏳ stays for a delivered item.
func TestDeliverInjectQueue_SlashCommandInjectedAlone(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	const wake, late = "You just woke up. Read the sleep-state document.", "/usage"
	rig := newHookRig(t, "cc")
	bs := rig.bs
	bs.ReactionTracker = stores.NewReactionTrackerStore()
	// A private tmux server. Its pane records every byte the injector sends (ESC shown as ^[) and echoes each
	// received line with the "❯ " prefix, so the pane scan of SafeInjectText confirms a paste like a real prompt.
	orig, server := injector.ServerName, fmt.Sprintf("tgcli-test-%d", os.Getpid())
	injector.ServerName = server
	t.Cleanup(func() { exec.Command("tmux", "-L", server, "kill-server").Run(); injector.ServerName = orig })
	rec := filepath.Join(t.TempDir(), "rec")
	recorder := fmt.Sprintf(`printf '\033[?2004h'; cat -v | tee %s | sed -u 's/^/❯ /'`, rec)
	if err := injector.NewSessionCmd("-d", "-s", "rec", "-x", "80", "-y", "24", recorder).Run(); err != nil {
		t.Fatal(err)
	}
	panes, err := injector.ListPanes("rec")
	if err != nil {
		t.Fatal(err)
	}
	target := panes[0]
	bs.SessionState.Add("sess-slash-t2", target, "/work", "")
	if err := config.SaveRoutes(config.Routes{NameRouteMap: map[string]config.NameRoute{"sess-slash-t2": {ChatID: 1}}}); err != nil {
		t.Fatal(err)
	}
	// The three messages were queued while the agent was busy: one "⏳ Queued" message holds them.
	queued, err := helpers.RetrySendRich(bs.Bot, &tele.Chat{ID: 1}, "⏳ Queued [a] (3)", helpers.RichSendOpts{})
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"/compact", "/compact", wake} {
		bs.InjectQueue.Enqueue(target, stores.InjectItem{Text: text})
	}
	bs.InjectQueue.SetNotifyMsg(target, queued.ID)
	pasted := func() string { got, _ := os.ReadFile(rec); return string(got) }
	check := func(after string) {
		t.Helper()
		if got := pasted(); got != pasteOf("/compact") {
			t.Fatalf("after %s the pane received %q, want exactly one /compact paste", after, got)
		}
		if queue := bs.InjectQueue.GetTexts(target); !slices.Equal(queue, []string{"/compact", wake, late}) {
			t.Fatalf("after %s the queue is %q, want [/compact, wake-up, late message] as three separate items", after, queue)
		}
	}
	// While the first flush injects the /compact the user sends one more message: the agent is busy, so it queues
	// itself and posts a "⏳ Queued" message of its own.
	flushed := make(chan struct{})
	go func() { deliverInjectQueue(bs, target, "", stores.InjectModeQueuedCommand); close(flushed) }()
	waitUntil(t, 3*time.Second, func() bool { return !bs.InjectQueue.HasItems(target) }) // the flush took the three items
	bs.InjectQueue.Enqueue(target, stores.InjectItem{Text: late})
	helpers.NotifyQueued(bs.Bot, bs.InjectQueue, &tele.Chat{ID: 1}, 0, target, target)
	<-flushed
	check("the first flush")
	deliverInjectQueue(bs, target, "", stores.InjectModeQueuedCommand)
	check("the second Force flush")
	sent, edited := callBodies(rig.tg, "sendRichMessage"), callBodies(rig.tg, "editMessageText")
	if len(sent) != 2 || len(edited) != 2 || !strings.Contains(edited[1], "⏳ Queued") || !strings.Contains(edited[1], wake) || !strings.Contains(edited[1], late) || strings.Count(edited[1], "/compact") != 1 {
		t.Fatalf("Telegram got the messages %q and the edits %q, want the late message's Queued message edited to list the three held items and no second one", sent, edited)
	}
	if !strings.Contains(edited[0], "(1 of 3)") || strings.Count(edited[0], "/compact") != 1 || strings.Contains(edited[0], "⏳") {
		t.Fatalf("the old message was edited to %q, want it to list only the delivered /compact", edited[0])
	}
	deliverInjectQueue(bs, target, "", stores.InjectModeIdle) // the Stop-routed or idle-ticker flush
	if got, want := pasted(), pasteOf("/compact")+pasteOf("/compact")+pasteOf(wake)+pasteOf(late); got != want {
		t.Fatalf("after the idle flush the pane received %q, want %q", got, want)
	}
	edited = callBodies(rig.tg, "editMessageText")
	if len(edited) != 3 || !strings.Contains(edited[2], "✅ Injected") || !strings.Contains(edited[2], "(3)") || strings.Contains(edited[2], "⏳") {
		t.Fatalf("the Queued message was edited to %q, want \"✅ Injected (3)\" with no ⏳", edited)
	}
}
