package injector

import (
	"os/exec"
	"reflect"
	"testing"
)

func TestParsePaneList(t *testing.T) {
	// Real `list-panes -a` output: pane_id \t pane_current_command \t pane_pid \t pane_title.
	// Titles contain spaces AND non-ASCII (e.g. "✳ Hell again"); the last field is captured whole by a
	// TAB split. A whitespace split would break every multi-word title.
	out := "%1\tclaude\t1001\t✳ Hell again\n" +
		"%2\tnode\t1002\tcodex on main\n" +
		"%3\tzsh\t1003\t\n"
	m := parsePaneList(out)
	if len(m) != 3 {
		t.Fatalf("parsePaneList: got %d panes, want 3", len(m))
	}
	// Title with spaces and non-ASCII preserved verbatim. This assertion FAILS if the parser is
	// changed to split on whitespace (the title would become "✳").
	if got := m["%1"].Title; got != "✳ Hell again" {
		t.Errorf("%%1 Title = %q, want %q (a whitespace split would yield \"✳\")", got, "✳ Hell again")
	}
	if got := m["%1"].Command; got != "claude" {
		t.Errorf("%%1 Command = %q, want claude", got)
	}
	if got := m["%1"].PID; got != "1001" {
		t.Errorf("%%1 PID = %q, want 1001", got)
	}
	// A row whose pane_current_command is "node".
	if got := m["%2"].Command; got != "node" {
		t.Errorf("%%2 Command = %q, want node", got)
	}
	if got := m["%2"].Title; got != "codex on main" {
		t.Errorf("%%2 Title = %q, want %q", got, "codex on main")
	}
	// An empty title parses to a present entry with an empty Title (not a missing entry).
	if got, ok := m["%3"]; !ok || got.Title != "" {
		t.Errorf("%%3 = %+v ok=%v, want present with empty Title", got, ok)
	}
	// A pane_id never emitted is absent from the map.
	if _, ok := m["%99"]; ok {
		t.Errorf("%%99 should be absent from the parsed map")
	}
}

// TestListPanesBatchOneCallPerSocket is the property Item 9 exists for: ListPanesBatch must issue
// exactly ONE exec per DISTINCT tmux socket per call (never one per session). It replaces the seam
// paneListCmd with a spy that records the real production argv and returns a harmless empty-output
// command (no real tmux), then restores the seam via defer. Feeding 5 targets across 2 sockets (3 share
// sockA, 2 share sockB) must produce exactly 2 calls, and each call's COMPLETE argv must equal
// `tmux -u [-L <server>] -S <socket> list-panes -a -F <tab format>` (full args asserted, not just the
// tail, so a dropped -u or a duplicated prefix flag fails too). FAILS if anyone reintroduces a
// per-session exec or changes any flag or the format.
func TestListPanesBatchOneCallPerSocket(t *testing.T) {
	orig := paneListCmd
	defer func() { paneListCmd = orig }()

	var calls []*exec.Cmd
	paneListCmd = func(socket string) *exec.Cmd {
		calls = append(calls, orig(socket)) // record the exact argv production would run
		return exec.Command("true")         // harmless: exit 0, empty output, no real tmux
	}

	targets := []TmuxTarget{
		{PaneID: "%1", Socket: "/tmp/sockA"},
		{PaneID: "%2", Socket: "/tmp/sockA"},
		{PaneID: "%3", Socket: "/tmp/sockB"},
		{PaneID: "%4", Socket: "/tmp/sockB"},
		{PaneID: "%5", Socket: "/tmp/sockA"},
	}
	ListPanesBatch(targets)

	if len(calls) != 2 {
		t.Fatalf("paneListCmd called %d times, want 2 (exactly one per distinct socket)", len(calls))
	}

	wantFormat := "#{pane_id}\t#{pane_current_command}\t#{pane_pid}\t#{pane_title}"
	gotSockets := map[string]bool{}
	for _, c := range calls {
		args := c.Args
		// Identify which socket this call targets via its -S value.
		socket := ""
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "-S" {
				socket = args[i+1]
				break
			}
		}
		if socket != "/tmp/sockA" && socket != "/tmp/sockB" {
			t.Errorf("call argv %v has unexpected -S socket %q", args, socket)
			continue
		}
		gotSockets[socket] = true
		// Assert the COMPLETE argv, not just the tail. tmuxCmd prepends -u, then -L <ServerName> when set,
		// then -S <socket>; a dropped -u or a duplicated prefix flag must fail here.
		wantArgs := []string{"tmux", "-u"}
		if ServerName != "" {
			wantArgs = append(wantArgs, "-L", ServerName)
		}
		wantArgs = append(wantArgs, "-S", socket, "list-panes", "-a", "-F", wantFormat)
		if !reflect.DeepEqual(args, wantArgs) {
			t.Errorf("call argv = %v, want %v", args, wantArgs)
		}
	}
	if len(gotSockets) != 2 || !gotSockets["/tmp/sockA"] || !gotSockets["/tmp/sockB"] {
		t.Errorf("distinct sockets called = %v, want exactly /tmp/sockA and /tmp/sockB", gotSockets)
	}
}
