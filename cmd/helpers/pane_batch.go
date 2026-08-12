package helpers

import (
	"os/exec"
	"strings"

	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/internal/injector"
)

// PaneState derives (title, running) for a tmux target from a pre-fetched pane map
// (injector.ListPanesBatch) and a pre-fetched children map (ResolvePaneChildren), replicating
// GetPaneTitle + IsSessionRunning with ZERO exec of its own. A target absent from the map (pane gone)
// yields ("", false) — the same empty-title / not-running result the old GetPaneTitle error path
// produced, so the caller's inject-queue-flush branch still runs. A "node" pane resolves its real
// backend by looking its shell pid up in children (built once per tick), so no per-pane ps runs here.
// The busy decision routes through storeOrTitleBusy (store-aware for pi; title-based for cc/codex/other),
// so a running pi session — which TitleIsBusy cannot classify — reads busy from the in-memory run-state
// store. A pi pane whose process died self-heals to idle: on the next tick Command != "pi" -> detectBackend
// returns "" -> the store is bypassed -> TitleIsBusy("", title) == false.
func PaneState(tmuxTarget string, panes map[string]injector.PaneInfo, children map[string]string, hookRunning *stores.HookRunningStateStore) (string, bool) {
	target, err := injector.ParseTarget(tmuxTarget)
	if err != nil {
		return "", false
	}
	info, ok := panes[injector.FormatTarget(target)]
	if !ok {
		return "", false
	}
	// Presence check, NOT a busy rule (bare == "" like the pre-refactor code, no trim — so it does not
	// touch the one-place TitleIsBusy ruling). Kept for parity with IsSessionRunning's empty-title guard;
	// it also skips the children-map lookup + switch for an empty-title pane.
	if info.Title == "" {
		return info.Title, false
	}
	// The busy decision routes through storeOrTitleBusy (store-aware for pi; title-based for cc/codex/other);
	// the backend comes from the shared detectBackend switch (its closure resolves a "node" pane from the
	// pre-fetched children map — NOT a live ps call — and is not invoked for claude/codex/other/pi). The
	// returned title is info.Title UNCHANGED — this getter must not normalize (it only feeds typingLog); the
	// whitespace trim for the decision lives only inside TitleIsBusy. tmuxTarget (the raw string arg) is the
	// store key — the same bare pane-id the agent_start hook uses in bs.HookRunning.SetRunning.
	backend := detectBackend(info.Command, func() string { return children[info.PID] })
	running := storeOrTitleBusy(hookRunning, backend, tmuxTarget, info.Title)
	return info.Title, running
}

// ResolvePaneChildren returns a map of shell-pid -> child command line(s) for the panes in a batch,
// built from ONE `ps -e -o ppid=,cmd=` call per tick (mirroring ListPanesBatch's one-exec-per-tick). It
// is the batched replacement for the per-"node"-pane `ps --ppid <pid>` fan-out, which was the last
// per-session-growing exec in the typing tick: `ps --ppid` still walks all of /proc, so N node panes
// cost N full /proc walks, while one `ps -e` costs a single walk regardless of session count. ps runs
// ONLY when at least one pane's command is "node" (the only case detectBackend resolves via the child
// cli); a pure-claude/codex/other tick pays ZERO ps. On ps error it returns an empty map (every node
// pane then resolves to "" — the same empty result the old ps error path produced).
func ResolvePaneChildren(panes map[string]injector.PaneInfo) map[string]string {
	hasNode := false
	for _, info := range panes {
		if info.Command == "node" {
			hasNode = true
			break
		}
	}
	if !hasNode {
		return map[string]string{}
	}
	out, err := psChildrenCmd().Output()
	if err != nil {
		return map[string]string{}
	}
	return parsePSChildren(string(out))
}

// psChildrenCmd builds the single batched `ps -e -o ppid=,cmd=` command that lists every process's
// parent pid and command line in ONE /proc walk. Package-level var so a test can replace it to count
// invocations (exactly one per tick when node panes exist, zero otherwise).
var psChildrenCmd = func() *exec.Cmd {
	return exec.Command("ps", "-e", "-o", "ppid=,cmd=")
}

// parsePSChildren parses `ps -e -o ppid=,cmd=` output into map[ppid]childCommands. Each line is trimmed
// (the ppid column is space-padded), the ppid is the text up to the first space/tab, and the trimmed
// remainder is one child's command line. A shell can have MORE THAN ONE child, so all children of the
// same ppid are accumulated joined by newline — the same set of tokens the old `ps --ppid` path scanned,
// whose whole multi-line blob was TrimSpace'd and then strings.Fields-split across every child. A ppid
// with no children is simply absent, so a lookup returns "" — the old empty-ps-output result.
func parsePSChildren(out string) map[string]string {
	m := make(map[string]string)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		i := strings.IndexAny(line, " \t")
		if i < 0 {
			continue
		}
		ppid := line[:i]
		cmd := strings.TrimSpace(line[i+1:])
		if prev, ok := m[ppid]; ok {
			m[ppid] = prev + "\n" + cmd
		} else {
			m[ppid] = cmd
		}
	}
	return m
}

// psCliCommandForPID returns the CLI command line of the child process(es) of shellPID via
// `ps --ppid <shellPID>`. It now serves ONLY the live path (GetPaneCLICommand, which fetches the pid via
// tmux then delegates here for a single target); the batched tick path resolves node panes from the
// pre-fetched children map (ResolvePaneChildren) instead. Returns "" on empty pid or ps error. It stays
// a package-level var so a test can replace it to prove the batched PaneState never falls back to this
// live per-target ps call.
var psCliCommandForPID = func(shellPID string) string {
	if shellPID == "" {
		return ""
	}
	out, err := exec.Command("ps", "--ppid", shellPID, "-o", "cmd", "--no-headers").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
