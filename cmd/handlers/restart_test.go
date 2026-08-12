package handlers

import (
	"testing"

	"github.com/Seraphli/tg-cli/internal/injector"
)

// TestExitCommandForCLI covers Round-4 Item 6: the exit-and-resume flow must be backend-aware. pi has NO
// /exit (its built-ins are compact/new/quit/reload) — sending "/exit" to pi is taken as a normal user prompt
// (the production /reload bug), so pi must be quit with /quit; cc and codex keep /exit (both proven). RED on
// the pre-fix behaviour (a hardcoded "/exit" for every backend -> the pi case fails).
func TestExitCommandForCLI(t *testing.T) {
	cases := []struct {
		cmd  string
		want string
	}{
		{"pi --continue", "/quit"},
		{"pi", "/quit"},
		{"/usr/local/bin/pi --resume", "/quit"},
		{"claude --continue", "/exit"},
		{"claude --resume --permission-mode plan", "/exit"},
		{"codex --continue", "/exit"},
		{"node /path/to/something", "/exit"},
		{"", "/exit"},
	}
	for _, c := range cases {
		if got := exitCommandForCLI(c.cmd); got != c.want {
			t.Errorf("exitCommandForCLI(%q) = %q, want %q", c.cmd, got, c.want)
		}
	}
}

// TestRestartSequenceTimeoutAborts covers Round-4 Item 6: on a SessionEnd timeout the restart flow must ABORT
// and MUST NOT inject the launch command (the old "proceeding anyway" typed the launch command into a live
// pane -> a second junk prompt). On a successful exit it injects the restart command exactly once. RED on the
// pre-fix behaviour (inject runs regardless of the exit result -> the timeout case injects).
func TestRestartSequenceTimeoutAborts(t *testing.T) {
	var sentKeys []string
	var injected []string
	sendKeys := func(_ injector.TmuxTarget, keys ...string) error { sentKeys = append(sentKeys, keys...); return nil }
	inject := func(cmd string) error { injected = append(injected, cmd); return nil }

	// timeout: waitExit returns false -> abort, no inject, error returned.
	err := restartSequence(injector.TmuxTarget{}, "%1@srv", "/quit", "pi --continue",
		sendKeys, func() bool { return false }, inject)
	if err == nil {
		t.Fatal("expected an error on timeout abort")
	}
	if len(injected) != 0 {
		t.Errorf("timeout must NOT inject the launch command, got %v", injected)
	}
	if len(sentKeys) == 0 || sentKeys[0] != "/quit" {
		t.Errorf("expected /quit sent first, got %v", sentKeys)
	}

	// success: waitExit returns true -> inject the restart command once, no error.
	sentKeys, injected = nil, nil
	err = restartSequence(injector.TmuxTarget{}, "%1@srv", "/exit", "claude --continue",
		sendKeys, func() bool { return true }, inject)
	if err != nil {
		t.Fatalf("unexpected error on success: %v", err)
	}
	if len(injected) != 1 || injected[0] != "claude --continue" {
		t.Errorf("expected exactly one restart inject of the launch command, got %v", injected)
	}
	if len(sentKeys) == 0 || sentKeys[0] != "/exit" {
		t.Errorf("expected /exit sent first, got %v", sentKeys)
	}
}

// TestStripBotMention covers Round-4 Item 6: /reload is no longer intercepted as a bot command, so the literal
// command must reach the pane — but Telegram appends @botname to a slash-command's first token in groups. The
// suffix is stripped from the first token only; non-slash text and unrelated @mentions are untouched.
func TestStripBotMention(t *testing.T) {
	const bot = "mybot"
	cases := []struct {
		in, want string
	}{
		{"/reload@mybot", "/reload"},
		{"/reload@mybot extra args", "/reload extra args"},
		{"/reload", "/reload"},
		{"/compact@mybot", "/compact"},
		{"hello @mybot world", "hello @mybot world"}, // non-slash: untouched
		{"/reload@otherbot", "/reload@otherbot"},     // different bot: untouched
		{"", ""},
	}
	for _, c := range cases {
		if got := stripBotMention(c.in, bot); got != c.want {
			t.Errorf("stripBotMention(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// Empty username: no-op.
	if got := stripBotMention("/reload@mybot", ""); got != "/reload@mybot" {
		t.Errorf("stripBotMention with empty username should be a no-op, got %q", got)
	}
}

// TestIsRestartCommand covers the headline of Round-4 Item 6: the bot intercepts /restart and its aliases
// (/rs, /r) — with or without the @botname mention Telegram appends in groups — and routes them to the
// exit-and-resume flow, while /reload (and /reload@bot) is NO LONGER intercepted so the literal reaches the
// pane for the backend's own hot reload. isRestartCommand is the single predicate the three routing sites
// share, so a future refactor that re-adds /reload to the intercepted set (the pre-fix bug) is caught here.
// RED on that regression: making isRestartCommand also match /reload fails the not-intercepted cases below.
func TestIsRestartCommand(t *testing.T) {
	intercepted := []string{"/restart", "/restart@mybot", "/rs", "/rs@mybot", "/r", "/r@mybot"}
	for _, s := range intercepted {
		if !isRestartCommand(s) {
			t.Errorf("isRestartCommand(%q) = false, want true (must be intercepted as a restart)", s)
		}
	}
	notIntercepted := []string{"/reload", "/reload@mybot", "/reload@mybot extra", "/reloadx", "/restartx", "/hello", "hi", ""}
	for _, s := range notIntercepted {
		if isRestartCommand(s) {
			t.Errorf("isRestartCommand(%q) = true, want false (must NOT be intercepted)", s)
		}
	}
}
