package injector

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const helperSocketEnv = "NEW_SESSION_HELPER_SOCKET"

// TestNewSessionHelper is the service process of TestNewSessionCmdSurvivesServiceStop: it starts the tmux server
// the way the bot does and then stays alive like the bot. In a normal test run it skips.
func TestNewSessionHelper(t *testing.T) {
	sock := os.Getenv(helperSocketEnv)
	if sock == "" {
		t.Skip("helper process of TestNewSessionCmdSurvivesServiceStop")
	}
	ServerName = sock
	if err := NewSessionCmd("-d", "-s", "scope-test", "-x", "80", "-y", "24").Run(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Minute)
}

// TestNewSessionCmdSurvivesServiceStop: `tg-cli service upgrade` stops tg-cli.service while agents run in tmux
// panes. A tmux server started by a systemd service through NewSessionCmd must keep its panes when that service
// stops. The test runs its own throwaway user service and a private tmux socket; it never touches tg-cli.service
// or the live tmux server.
func TestNewSessionCmdSurvivesServiceStop(t *testing.T) {
	for _, tool := range []string{"tmux", "systemd-run", "systemctl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	if err := exec.Command("systemctl", "--user", "show-environment").Run(); err != nil {
		t.Skip("no systemd user manager")
	}
	name := fmt.Sprintf("tgcli-scope-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	unit := name + ".service"
	tmuxDir := os.Getenv("TMUX_TMPDIR")
	if tmuxDir == "" {
		tmuxDir = "/tmp"
	}
	panes := func() string {
		out, _ := exec.Command("tmux", "-L", name, "list-panes", "-a").Output()
		return strings.TrimSpace(string(out))
	}
	t.Cleanup(func() {
		exec.Command("tmux", "-L", name, "kill-server").Run()
		exec.Command("systemctl", "--user", "stop", unit).Run()
		exec.Command("systemctl", "--user", "reset-failed", unit).Run()
		os.Remove(filepath.Join(tmuxDir, fmt.Sprintf("tmux-%d", os.Getuid()), name))
	})
	if out, err := exec.Command("systemd-run", "--user", "--quiet", "--unit="+name, "--collect",
		"--setenv="+helperSocketEnv+"="+name, os.Args[0], "-test.run=^TestNewSessionHelper$").CombinedOutput(); err != nil {
		t.Fatalf("start the throwaway service: %v: %s", err, out)
	}
	deadline := time.Now().Add(5 * time.Second)
	for panes() == "" && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if panes() == "" {
		t.Fatal("the service never started a tmux pane")
	}
	if out, err := exec.Command("systemctl", "--user", "stop", unit).CombinedOutput(); err != nil {
		t.Fatalf("stop the throwaway service: %v: %s", err, out)
	}
	time.Sleep(time.Second)
	if panes() == "" {
		t.Fatal("the pane died with the service")
	}
}
