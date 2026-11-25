package helpers

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Seraphli/tg-cli/internal/injector"
)

// liveNeeds are the environment variables a live test needs per CLI; the test is skipped unless TG_CLI_LIVE=1 and all
// of them are set (cc also needs the credential file $HOME/.claude/.credentials.json). The binaries are the real ones,
// not the ~/.local/agent-bin wrappers (those need a user systemd bus).
var liveNeeds = map[string][]string{
	"cc":    {"TG_CLI_LIVE_CC_BIN"},
	"codex": {"CODEX_BASE_URL", "CODEX_API_KEY", "CODEX_MODEL", "TG_CLI_LIVE_CODEX_BIN"},
	"pi":    {"PI_E2E_PROVIDER", "PI_E2E_BASE_URL", "PI_E2E_MODEL", "NEWAPI_E2E_KEY", "TG_CLI_LIVE_PI_BIN"},
}

// liveCfgDir is the private config dir of the CLI started by the last liveStart; the live tests run one at a time.
var liveCfgDir string

// liveTail keeps the pane open for 60 s after the CLI exits, so a failed start can still be captured. It sits inside the
// quoted command of `bash -i -c`, so bash does not exec the CLI and stays its parent, which DetectBackend needs: it
// reads the CLI's command line through `ps --ppid`. The interactive bash job-controls the CLI like a production pane
// shell does, so the CLI stays the foreground process while a tool runs (under a plain `sh -c` the tool's zsh becomes the
// foreground process, #{pane_current_command} reads "zsh", DetectBackend returns "" and LiveBusyState reads a busy cc as idle).
const liveTail = `; echo "[exited rc=$?]"; sleep 60`

// shellJoin quotes every word for the pane shell (like printf %q), so values with quotes survive intact.
func shellJoin(words ...string) string {
	q := make([]string, len(words))
	for i, w := range words {
		q[i] = "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
	}
	return strings.Join(q, " ")
}

func liveWrite(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// liveCopy copies a file without reading it into the test: the cc credential file never reaches a log.
func liveCopy(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
}

// liveFail saves the pane (scrollback included) to the test log and fails the test.
func liveFail(t *testing.T, target injector.TmuxTarget, format string, args ...any) {
	t.Helper()
	pane, _ := injector.CapturePane(target)
	t.Logf("pane when the test failed:\n%s", pane)
	t.Fatalf(format, args...)
}

// liveStart starts the real CLI ("cc", "codex" or "pi") in a private tmux server (tglive-<pid>-<cli>, never the
// default socket) with a Go temp dir as working directory and an isolated config dir, points injector.ServerName at
// that server, waits until the CLI is idle and returns its pane. ccPerm is cc's --permission-mode (ignored for the other
// CLIs). Nothing of tg-cli runs in the pane: the config dir holds no hook and no extension, and the environment of the
// pane is an allowlist without TG_CLI_* (and for cc without ANTHROPIC_*). Secrets reach the CLI through the environment
// of the tmux server only, never through argv. The pane command is `bash --norc --noprofile -i -c '<cli>; ...'`: the
// CLI is a child of an interactive bash with job control, as in a production pane, so it stays the foreground process
// while a tool runs and DetectBackend still sees it. t.Cleanup kills the server, restores injector.ServerName and
// removes the two temp dirs (os.MkdirTemp, not t.TempDir: the CLI can still write into them for a moment after the kill,
// so the removal is retried for up to 5 s).
func liveStart(t *testing.T, cli string, rows int, ccPerm string) injector.TmuxTarget {
	t.Helper()
	if os.Getenv("TG_CLI_LIVE") != "1" {
		t.Skip("live test: set TG_CLI_LIVE=1")
	}
	for _, name := range liveNeeds[cli] {
		if os.Getenv(name) == "" {
			t.Skipf("live test: %s is not set", name)
		}
	}
	home := os.Getenv("HOME")
	cfg, err := os.MkdirTemp("", "tglive-")
	if err != nil {
		t.Fatal(err)
	}
	work, err := os.MkdirTemp("", "tglive-")
	if err != nil {
		os.RemoveAll(cfg)
		t.Fatal(err)
	}
	dirs := []string{cfg, work}
	liveCfgDir = cfg
	server := fmt.Sprintf("tglive-%d-%s", os.Getpid(), cli)
	orig := injector.ServerName
	injector.ServerName = server
	t.Cleanup(func() {
		exec.Command("tmux", "-L", server, "kill-server").Run()
		injector.ServerName = orig
		for _, dir := range dirs {
			var err error
			for i := 0; i < 20; i++ {
				if err = os.RemoveAll(dir); err == nil {
					break
				}
				time.Sleep(250 * time.Millisecond)
			}
			if err != nil {
				t.Logf("remove %s after 20 tries: %v", dir, err)
			}
		}
	})
	if work, err = filepath.EvalSymlinks(work); err != nil {
		t.Fatal(err)
	}
	env := []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "TERM=xterm-256color", "LANG=en_US.UTF-8"}
	var launch string
	var idle func(string) bool
	switch cli {
	case "cc":
		cred := filepath.Join(home, ".claude", ".credentials.json")
		if _, err := os.Stat(cred); err != nil {
			t.Skip("live test: no cc credential file")
		}
		liveCopy(t, cred, filepath.Join(cfg, ".credentials.json"))
		liveWrite(t, cfg, ".claude.json", fmt.Sprintf(`{"hasCompletedOnboarding":true,"hasInitOnboardingBeenShown":true,"numStartups":100,"autoUpdates":false,"projects":{%q:{"hasTrustDialogAccepted":true}}}`, work))
		// the boss's cc runs with this setting; without it cc picks its renderer by server-side gates, and on the classic renderer a closed menu leaves only the tail in the viewport
		liveWrite(t, cfg, "settings.json", `{"permissions":{"allow":["Bash(python3 *)"]},"tui":"fullscreen"}`)
		launch = "env -i " + shellJoin(env...) + " " + shellJoin("BROWSER=none", "DISABLE_AUTOUPDATER=1", "CLAUDE_CONFIG_DIR="+cfg) + " " +
			shellJoin(os.Getenv("TG_CLI_LIVE_CC_BIN"), "--permission-mode", ccPerm, "--setting-sources", "user", "--no-chrome", "--tools", "Bash,Read,Write,TaskList")
		idle = func(v string) bool { _, ok := ccBoxText(v); return ok }
	case "codex":
		launch = shellJoin(os.Getenv("TG_CLI_LIVE_CODEX_BIN"), "--model", os.Getenv("CODEX_MODEL"),
			"-c", `model_provider="custom"`,
			"-c", `model_providers.custom.name="Custom E2E"`,
			"-c", "model_providers.custom.base_url="+strconv.Quote(os.Getenv("CODEX_BASE_URL")),
			"-c", `model_providers.custom.env_key="CODEX_API_KEY"`,
			"-c", `model_providers.custom.wire_api="responses"`,
			"--yolo", "--disable", "shell_snapshot")
		env = append(env, "CODEX_HOME="+cfg, "CODEX_API_KEY="+os.Getenv("CODEX_API_KEY"), "DISABLE_AUTOUPDATER=1")
		idle = func(v string) bool { return strings.Contains(v, codexPlaceholder) }
	case "pi":
		provider, model := os.Getenv("PI_E2E_PROVIDER"), os.Getenv("PI_E2E_MODEL")
		// the apiKey is the literal string "$NEWAPI_E2E_KEY": pi reads the variable of that name from its environment
		liveWrite(t, cfg, "models.json", fmt.Sprintf(`{"providers":{%q:{"baseUrl":%q,"api":"openai-completions","apiKey":"$NEWAPI_E2E_KEY","models":[{"id":%q,"name":%q,"reasoning":false,"input":["text"],"contextWindow":65536,"maxTokens":8192,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0}}]}}}`,
			provider, os.Getenv("PI_E2E_BASE_URL"), model, model))
		liveWrite(t, cfg, "auth.json", "{}")
		liveWrite(t, cfg, "settings.json", `{"compaction":{"reserveTokens":32768}}`)
		launch = shellJoin(os.Getenv("TG_CLI_LIVE_PI_BIN"), "--provider", provider, "--model", model)
		env = append(env, "PI_CODING_AGENT_DIR="+cfg, "PI_OFFLINE=1", "DISABLE_AUTOUPDATER=1", "NEWAPI_E2E_KEY="+os.Getenv("NEWAPI_E2E_KEY"))
		idle = func(v string) bool {
			text, ok := piBoxText(v)
			return ok && text == ""
		}
	default:
		t.Fatalf("unknown live CLI %q", cli)
	}
	start := exec.Command("tmux", "-u", "-L", server, "-f", "/dev/null", "new-session", "-d", "-s", "live", "-x", "200", "-y", strconv.Itoa(rows), "-c", work, "bash --norc --noprofile -i -c "+shellJoin(launch+liveTail))
	start.Env = env
	if out, err := start.CombinedOutput(); err != nil {
		t.Fatalf("start %s in tmux server %s: %v: %s", cli, server, err, out)
	}
	paneID, err := exec.Command("tmux", "-L", server, "display-message", "-p", "-t", "live", "#{pane_id}").Output()
	if err != nil {
		t.Fatalf("read the pane id of tmux server %s: %v", server, err)
	}
	target := injector.TmuxTarget{PaneID: strings.TrimSpace(string(paneID))}
	ready := time.Now()
	liveWait(t, target, 90*time.Second, idle)
	t.Logf("live %s: ready after %.1f s", cli, time.Since(ready).Seconds())
	if cli == "cc" {
		alt, err := exec.Command("tmux", "-L", server, "display-message", "-p", "-t", target.PaneID, "#{alternate_on}").Output()
		if err != nil {
			t.Fatalf("read alternate_on of pane %s on tmux server %s: %v", target.PaneID, server, err)
		}
		if v := strings.TrimSpace(string(alt)); v != "1" {
			liveFail(t, target, "cc is not on the fullscreen renderer: alternate_on=%q", v)
		}
	}
	var names []string
	entries, _ := os.ReadDir(cfg)
	for _, e := range entries {
		names = append(names, e.Name())
	}
	t.Logf("live %s: the private config dir holds %v", cli, names)
	return target
}

// hasReplyLine reports whether the viewport has a line whose text, after an optional ● or • bullet, is reply.
func hasReplyLine(v, reply string) bool {
	for _, ln := range strings.Split(v, "\n") {
		if strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(ln), "●•")) == reply {
			return true
		}
	}
	return false
}

// liveTurn injects prompt and waits up to 120 s for the reply line with a viewport that has not changed for 1.5 s
// (a running turn keeps its spinner and timer moving, so a still viewport is a finished turn).
func liveTurn(t *testing.T, target injector.TmuxTarget, prompt, reply string) {
	t.Helper()
	start := time.Now()
	if err := injector.InjectText(target, prompt); err != nil {
		t.Fatalf("inject %q: %v", prompt, err)
	}
	var prev string
	changed := time.Now()
	for deadline := changed.Add(120 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		v := viewportNow(target)
		if v != prev {
			prev, changed = v, time.Now()
		} else if time.Since(changed) >= 1500*time.Millisecond && hasReplyLine(v, reply) {
			t.Logf("live turn took %.1f s: %.40q", time.Since(start).Seconds(), prompt)
			return
		}
	}
	liveFail(t, target, "no finished turn with the reply line %q within 120 s", reply)
}

var liveDoneRe = regexp.MustCompile(`✻ .+ for [0-9]+`)

// liveSend injects prompt and waits for the turn to end by the pane (still viewport and cc's "✻ <verb> for <n>s" line), not by
// a reply line: cc's model does not follow a request inside a long paste.
func liveSend(t *testing.T, target injector.TmuxTarget, prompt string) {
	if err := injector.InjectText(target, prompt); err != nil {
		t.Fatalf("inject %.40q: %v", prompt, err)
	}
	var prev string
	changed := time.Now()
	for deadline := changed.Add(120 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if v := viewportNow(target); v != prev {
			prev, changed = v, time.Now()
		} else if time.Since(changed) >= 1500*time.Millisecond && liveDoneRe.MatchString(v) {
			return
		}
	}
	liveFail(t, target, "no finished turn within 120 s")
}

// liveWait polls the viewport until pred holds and returns it; on timeout it saves the pane to the log and fails the test.
func liveWait(t *testing.T, target injector.TmuxTarget, timeout time.Duration, pred func(string) bool) string {
	t.Helper()
	v, ok := pollViewport(target, timeout, pred)
	if !ok {
		liveFail(t, target, "condition not met within %s", timeout)
	}
	return v
}

// noRewindCheck is the check the live rewind tests pass to RewindList and RewindDrive: no bot and no stores exist
// there (the real check is rewindCheck in cmd/handlers).
func noRewindCheck() string { return "" }
