package helpers

import "testing"

// TestDetectBackendSwitch exercises the single shared switch detectBackend directly: the full command
// mapping AND the mandatory laziness of the cliCommand closure. The closure must be invoked ONLY for a
// "node" pane or a pane whose command is a version number (cc started from its version file) — claude/codex/
// other panes must never trigger the pid fetch / ps call, which is why the closure (not an eager string) is
// required. This is the switch the live path (DetectBackend, exercised by
// TestDetectBackendLiveEntry) routes through.
func TestDetectBackendSwitch(t *testing.T) {
	cases := []struct {
		name       string
		command    string
		cliCommand string
		want       string
		wantCalls  int
	}{
		{"claude maps to cc without resolving cli", "claude", "unused", "cc", 0},
		{"codex maps to codex without resolving cli", "codex", "unused", "codex", 0},
		{"other command maps to empty without resolving cli", "zsh", "unused", "", 0},
		{"empty command maps to empty without resolving cli", "", "unused", "", 0},
		{"node resolving a codex cli", "node", "/usr/bin/node /home/u/.local/bin/codex --flag", "codex", 1},
		{"node resolving a claude cli", "node", "/usr/bin/node /home/u/.local/bin/claude", "cc", 1},
		{"node with unrelated cli maps to empty", "node", "/usr/bin/node /home/u/app/server.js", "", 1},
		{"node with no child cli maps to empty", "node", "", "", 1},
		{"cc started from its version file", "2.1.319", "/home/u/.local/share/claude/versions/2.1.319 --model sonnet --setting-sources user --no-chrome --tools Bash,Read,AskUserQuestion,TaskList", "cc", 1},
	}
	for _, c := range cases {
		calls := 0
		got := detectBackend(c.command, func() string {
			calls++
			return c.cliCommand
		})
		if got != c.want {
			t.Errorf("%s: detectBackend(%q) = %q, want %q", c.name, c.command, got, c.want)
		}
		if calls != c.wantCalls {
			t.Errorf("%s: cliCommand closure called %d times, want %d (only node and version-number panes resolve the cli)", c.name, calls, c.wantCalls)
		}
	}
}

// TestDetectBackendLiveEntry exercises the live entry point DetectBackend through the shared switch. A
// tmux target on a socket with no running server can't resolve a command, so GetPaneCommand returns ""
// and the shared switch yields "" (its default) — proving DetectBackend delegates to detectBackend and
// that a non-"node" (here empty) command never triggers GetPaneCLICommand / ps. The cc/codex/node
// mappings on a real pane are covered directly by TestDetectBackendSwitch and by Stage-A E2E; a
// hermetic unit test can't feed DetectBackend a synthetic pane command without a live tmux server.
func TestDetectBackendLiveEntry(t *testing.T) {
	if got := DetectBackend("%999@/tmp/tg-cli-detectbackend-test-nosock"); got != "" {
		t.Errorf("DetectBackend(unresolvable target) = %q, want \"\"", got)
	}
}
