package helpers

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/internal/injector"
)

func TestPaneState(t *testing.T) {
	// Pre-fetched pane map (as injector.ListPanesBatch returns, keyed by FormatTarget).
	panes := map[string]injector.PaneInfo{
		"%1":  {Command: "claude", PID: "1001", Title: "✳ Hell again"}, // cc: classified from CONTENT (idle body)
		"%2":  {Command: "claude", PID: "1002", Title: "· Working"},    // cc: classified from CONTENT (busy body)
		"%3":  {Command: "codex", PID: "1003", Title: "⠙ project"},     // codex busy: braille prefix (title)
		"%4":  {Command: "codex", PID: "1004", Title: "project"},       // codex idle: no braille prefix (title)
		"%5":  {Command: "zsh", PID: "1005", Title: "shell"},           // unknown backend
		"%6":  {Command: "claude", PID: "1006", Title: ""},             // present but empty title (short-circuit)
		"%7":  {Command: "claude", PID: "1007", Title: "  ✳ Job  "},    // cc: classified from CONTENT (idle body)
		"%8":  {Command: "node", PID: "1008", Title: "⠙ project"},      // node running codex, busy braille (title)
		"%9":  {Command: "node", PID: "1009", Title: "✳ ready"},        // node running cc, classified from CONTENT
		"%10": {Command: "node", PID: "1010", Title: ""},               // node, empty title (short-circuit)
	}
	// Pre-fetched children map (as ResolvePaneChildren returns): shell pid -> child cli. Only "node" panes
	// consult it; claude/codex/zsh panes classify without it. 1010 is absent (empty-title short-circuit).
	children := map[string]string{
		"1008": "/usr/bin/node /home/u/.local/bin/codex --flag",
		"1009": "/usr/bin/node /home/u/.local/bin/claude",
	}
	// cc panes now derive their verdict from the LIVE pane CONTENT (ccBusyFromContent), not the title/store.
	// Stub the capture seam so cc panes classify from a supplied body; keyed by PaneID. Rules are 60 ─ (U+2500).
	ccBusyBody := "✻ Kneading…\n" + strings.Repeat("─", 60) + "\n❯ \n" + strings.Repeat("─", 60) + "\n"
	ccIdleBody := "✻ Cooked for 5s · done\n" + strings.Repeat("─", 60) + "\n❯ \n" + strings.Repeat("─", 60) + "\n"
	errFakeNoPane := errors.New("no fake pane")
	origCapture := captureViewport
	defer func() { captureViewport = origCapture }()
	captureViewport = func(ctx context.Context, target injector.TmuxTarget) (string, error) {
		switch target.PaneID {
		case "%2":
			return ccBusyBody, nil
		case "%1", "%7", "%9":
			return ccIdleBody, nil
		default:
			return "", errFakeNoPane
		}
	}
	cases := []struct {
		name        string
		target      string
		wantTitle   string
		wantRunning bool
	}{
		// cc verdict comes from CONTENT (ccIdleBody -> idle); the RETURNED title is still info.Title unchanged.
		{"cc idle from content keeps title", "%1", "✳ Hell again", false},
		{"cc busy from content is running", "%2", "· Working", true},
		{"codex busy is running", "%3", "⠙ project", true},
		{"codex idle not running", "%4", "project", false},
		{"unknown backend not running", "%5", "shell", false},
		{"present empty title not running", "%6", "", false},
		// cc verdict comes from CONTENT (ccIdleBody -> idle); the RETURNED title is the untrimmed original —
		// PaneState must not normalize.
		{"cc idle from content, title untrimmed", "%7", "  ✳ Job  ", false},
		// node panes resolve their real backend from the children map (the batched replacement for
		// `ps --ppid`): 1008 -> codex (busy braille title), 1009 -> cc (verdict from CONTENT, not the title).
		{"node->codex busy classified via children map", "%8", "⠙ project", true},
		{"node->cc idle classified via content", "%9", "✳ ready", false},
		{"node empty title short-circuits", "%10", "", false},
		// A pane_id queried but absent from the map → empty title, not running (the old GetPaneTitle
		// error path). This is the branch that lets flushInjectQueue run for a pane that is gone.
		{"absent pane yields empty and not-running", "%99", "", false},
	}
	// No pi panes here, so the run-state store is never consulted; a fresh empty store is enough.
	store := stores.NewHookRunningStateStore()
	for _, c := range cases {
		title, running := PaneState(context.Background(), c.target, panes, children, store)
		if title != c.wantTitle || running != c.wantRunning {
			t.Errorf("%s: PaneState(%q) = (%q, %v), want (%q, %v)", c.name, c.target, title, running, c.wantTitle, c.wantRunning)
		}
	}
}

// TestPaneStateUsesChildrenMapNotLivePs proves the batched path resolves a node pane's backend from the
// pre-fetched children map and NEVER falls back to the live per-target ps resolver (psCliCommandForPID),
// which must stay reserved for the single-target live path (GetPaneCLICommand). Replaces that live
// resolver with a counter: a node pane classified via the children map must leave it at zero. If
// PaneState is reverted to call psCliCommandForPID(info.PID), the counter fires.
func TestPaneStateUsesChildrenMapNotLivePs(t *testing.T) {
	orig := psCliCommandForPID
	defer func() { psCliCommandForPID = orig }()
	calls := 0
	psCliCommandForPID = func(shellPID string) string {
		calls++
		return "/usr/bin/node /home/u/.local/bin/codex"
	}
	panes := map[string]injector.PaneInfo{
		"%8": {Command: "node", PID: "1008", Title: "⠙ project"},
	}
	children := map[string]string{"1008": "/usr/bin/node /home/u/.local/bin/codex"}
	title, running := PaneState(context.Background(), "%8", panes, children, stores.NewHookRunningStateStore())
	if title != "⠙ project" || !running {
		t.Errorf("PaneState(node, codex child) = (%q, %v), want (%q, true)", title, running, "⠙ project")
	}
	if calls != 0 {
		t.Errorf("live ps resolver called %d times for a batched node pane, want 0 (PaneState must resolve from the children map, not the live ps)", calls)
	}
}

// TestPaneStateStoreAware covers the store-aware busy routing PaneState now uses (storeOrTitleBusy). A
// running pi session — which TitleIsBusy cannot classify (it only handles cc/codex) — must read busy from
// the in-memory run-state store; cc panes classify from the LIVE pane content and IGNORE the store; and a
// pi pane whose process died (current command no longer "pi") must self-heal to idle because detectBackend
// returns "" and the store is bypassed. It also proves the store-aware path never falls back to the live
// per-target ps resolver (psCliCommandForPID stays at zero invocations — the f29 no-live-exec invariant).
func TestPaneStateStoreAware(t *testing.T) {
	orig := psCliCommandForPID
	defer func() { psCliCommandForPID = orig }()
	psCalled := false
	psCliCommandForPID = func(shellPID string) string {
		psCalled = true
		return ""
	}
	panes := map[string]injector.PaneInfo{
		"%1": {Command: "pi", PID: "2001", Title: "pi session"},    // pi, store set RUNNING -> busy from store
		"%2": {Command: "pi", PID: "2002", Title: "pi session"},    // pi, store unknown -> title path -> idle
		"%3": {Command: "pi", PID: "2003", Title: "pi session"},    // pi, store set IDLE -> idle from store
		"%4": {Command: "claude", PID: "2004", Title: "· Working"}, // cc: classified from CONTENT (busy body); store ignored
		"%5": {Command: "claude", PID: "2005", Title: "✳ ready"},   // cc: classified from CONTENT (idle body); store ignored
		"%6": {Command: "bash", PID: "2006", Title: "· Working"},   // dead pi (command != "pi") + store RUNNING -> idle
	}
	children := map[string]string{}
	// cc panes derive their verdict from the LIVE pane CONTENT (ccBusyFromContent), not the store/title. Stub
	// the capture seam so cc panes classify from a supplied body; keyed by PaneID. Rules are 60 ─ (U+2500).
	ccBusyBody := "✻ Kneading…\n" + strings.Repeat("─", 60) + "\n❯ \n" + strings.Repeat("─", 60) + "\n"
	ccIdleBody := "✻ Cooked for 5s · done\n" + strings.Repeat("─", 60) + "\n❯ \n" + strings.Repeat("─", 60) + "\n"
	errFakeNoPane := errors.New("no fake pane")
	origCapture := captureViewport
	defer func() { captureViewport = origCapture }()
	captureViewport = func(ctx context.Context, target injector.TmuxTarget) (string, error) {
		switch target.PaneID {
		case "%4":
			return ccBusyBody, nil
		case "%5":
			return ccIdleBody, nil
		default:
			return "", errFakeNoPane
		}
	}
	store := stores.NewHookRunningStateStore()
	store.SetRunning("%1") // pi running
	// "%2" intentionally left unknown (never set) — store returns known=false, falls to the title path.
	store.SetIdle("%3")    // pi explicitly idle
	store.SetRunning("%4") // cc: store says running, but cc must classify from CONTENT
	store.SetRunning("%5") // cc: store says running, but an idle CONTENT body must win (store ignored for cc)
	store.SetRunning("%6") // dead pi: store still says running, but backend "" bypasses the store
	cases := []struct {
		name        string
		target      string
		wantTitle   string
		wantRunning bool
	}{
		// Core fix: a running pi session reads busy from the store (old TitleIsBusy returned false for pi).
		{"pi running from store", "%1", "pi session", true},
		{"pi unknown in store not running", "%2", "pi session", false},
		{"pi idle from store not running", "%3", "pi session", false},
		// Rider 3a: cc classifies from CONTENT, never the store.
		{"cc busy from content is running (store ignored)", "%4", "· Working", true},
		{"cc idle from content not running even though store says running", "%5", "✳ ready", false},
		// Rider 3b: a pi process that died shows a non-"pi" command -> detectBackend "" -> store bypassed.
		{"dead pi self-heals to idle despite store running", "%6", "· Working", false},
	}
	for _, c := range cases {
		title, running := PaneState(context.Background(), c.target, panes, children, store)
		if title != c.wantTitle || running != c.wantRunning {
			t.Errorf("%s: PaneState(%q) = (%q, %v), want (%q, %v)", c.name, c.target, title, running, c.wantTitle, c.wantRunning)
		}
	}
	if psCalled {
		t.Errorf("live ps resolver invoked during store-aware PaneState; want zero live per-target ps (f29 no-live-exec invariant)")
	}
}

// TestParsePSChildren covers the `ps -e -o ppid=,cmd=` parser: a ppid with MULTIPLE children (both
// present in the accumulated value, joined by newline — the same tokens the old ps --ppid multi-line blob
// carried), a ppid with one child, a tab-separated / space-padded ppid column, and an absent ppid
// resolving to "".
func TestParsePSChildren(t *testing.T) {
	// ps right-pads the ppid column; children of the same ppid appear on separate lines.
	out := "  1000 /usr/bin/node /home/u/.local/bin/codex\n" +
		"  1000 /usr/bin/helper --watch\n" +
		"  2000 zsh\n" +
		"\n" +
		" 3000\ttabbed-cmd\n"
	m := parsePSChildren(out)
	// Both children of 1000 are present and joined by newline — storing only the first would misclassify
	// a multi-child pane.
	got := m["1000"]
	if !strings.Contains(got, "codex") || !strings.Contains(got, "helper --watch") {
		t.Errorf("ppid 1000 = %q, want both children present", got)
	}
	if got != "/usr/bin/node /home/u/.local/bin/codex\n/usr/bin/helper --watch" {
		t.Errorf("ppid 1000 = %q, want the two children joined by newline", got)
	}
	// Single child.
	if got := m["2000"]; got != "zsh" {
		t.Errorf("ppid 2000 = %q, want %q", got, "zsh")
	}
	// A tab between ppid and cmd is also a valid separator; the ppid column is space-padded.
	if got := m["3000"]; got != "tabbed-cmd" {
		t.Errorf("ppid 3000 = %q, want %q", got, "tabbed-cmd")
	}
	// An absent ppid resolves to "" (zero value), matching the old empty ps output.
	if got := m["9999"]; got != "" {
		t.Errorf("absent ppid 9999 = %q, want \"\"", got)
	}
}

// TestResolvePaneChildrenBatchesOncePerTickWithNode is the property this item exists for: when any pane's
// command is "node", ResolvePaneChildren must exec ps EXACTLY ONCE per tick (never one `ps --ppid` per
// node pane). Replaces the psChildrenCmd seam with a counting spy returning a harmless empty-output
// command; two node panes must still produce exactly one batch.
func TestResolvePaneChildrenBatchesOncePerTickWithNode(t *testing.T) {
	orig := psChildrenCmd
	defer func() { psChildrenCmd = orig }()
	calls := 0
	psChildrenCmd = func() *exec.Cmd {
		calls++
		return exec.Command("true") // exit 0, empty output, no real ps
	}
	panes := map[string]injector.PaneInfo{
		"%1": {Command: "node", PID: "1001", Title: "⠙ a"},
		"%2": {Command: "node", PID: "1002", Title: "⠙ b"},
		"%3": {Command: "claude", PID: "1003", Title: "✳ c"},
	}
	ResolvePaneChildren(panes)
	if calls != 1 {
		t.Errorf("psChildrenCmd called %d times for a tick with node panes, want exactly 1 (one batched ps per tick, never one per node pane)", calls)
	}
}

// TestResolvePaneChildrenZeroPsWithoutNode proves a tick with NO "node" pane never execs ps at all — a
// pure-claude/codex/other machine pays zero /proc walks per tick.
func TestResolvePaneChildrenZeroPsWithoutNode(t *testing.T) {
	orig := psChildrenCmd
	defer func() { psChildrenCmd = orig }()
	calls := 0
	psChildrenCmd = func() *exec.Cmd {
		calls++
		return exec.Command("true")
	}
	panes := map[string]injector.PaneInfo{
		"%1": {Command: "claude", PID: "1001", Title: "✳ a"},
		"%2": {Command: "codex", PID: "1002", Title: "dir"},
		"%3": {Command: "zsh", PID: "1003", Title: "shell"},
	}
	got := ResolvePaneChildren(panes)
	if calls != 0 {
		t.Errorf("psChildrenCmd called %d times for a tick with no node pane, want 0", calls)
	}
	if len(got) != 0 {
		t.Errorf("ResolvePaneChildren(no node) = %v, want empty map", got)
	}
}

// TestDetectBackendSwitch exercises the single shared switch detectBackend directly: the full command
// mapping AND the mandatory laziness of the cliCommand closure. The closure must be invoked ONLY for a
// "node" pane — claude/codex/other panes must never trigger the pid fetch / ps call, which is why the
// closure (not an eager string) is required. This is the switch both entry points route through: the
// batched path (PaneState, exercised by TestPaneState) and the live path (DetectBackend, exercised by
// TestDetectBackendLiveEntry).
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
			t.Errorf("%s: cliCommand closure called %d times, want %d (non-node panes must not resolve the cli)", c.name, calls, c.wantCalls)
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
