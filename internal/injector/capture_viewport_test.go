package injector

import (
	"context"
	"os/exec"
	"testing"
)

// hasSocketPair reports whether args contains "-S" immediately followed by socket.
func hasSocketPair(args []string, socket string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-S" && args[i+1] == socket {
			return true
		}
	}
	return false
}

// hasScrollbackPair reports whether args contains a "-S" immediately followed by "-" (the capture-pane
// scrollback flag pair `-S -`). This is what CaptureViewport must NOT emit.
func hasScrollbackPair(args []string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-S" && args[i+1] == "-" {
			return true
		}
	}
	return false
}

func contains(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// TestCaptureViewportCmdArgs asserts captureViewportCmd mirrors tmuxCmd's GLOBAL socket prefix (-S
// target.Socket) but OMITS the capture-pane scrollback pair `-S -` that CapturePane carries. It also
// covers the -L ServerName branch by setting/restoring the package var.
func TestCaptureViewportCmdArgs(t *testing.T) {
	target := TmuxTarget{PaneID: "%3", Socket: "/tmp/sock"}

	cmd := captureViewportCmd(context.Background(), target)
	args := cmd.Args
	if !contains(args, "-u") {
		t.Errorf("args %v missing -u", args)
	}
	if !hasSocketPair(args, "/tmp/sock") {
		t.Errorf("args %v missing global socket pair -S /tmp/sock", args)
	}
	if !contains(args, "-p") {
		t.Errorf("args %v missing -p", args)
	}
	if !contains(args, "capture-pane") {
		t.Errorf("args %v missing capture-pane", args)
	}
	// The whole point: the scrollback pair `-S -` must be absent (only the visible viewport).
	if hasScrollbackPair(args) {
		t.Errorf("args %v must NOT contain the scrollback pair -S -", args)
	}

	// Contrast against CapturePane, which DOES carry the scrollback pair.
	capArgs := tmuxCmd(target, "capture-pane", "-t", target.PaneID, "-p", "-S", "-").Args
	if !hasScrollbackPair(capArgs) {
		t.Errorf("CapturePane args %v should contain the scrollback pair -S - (contrast baseline)", capArgs)
	}

	// Cover the -L ServerName branch.
	origServer := ServerName
	defer func() { ServerName = origServer }()
	ServerName = "myserver"
	lArgs := captureViewportCmd(context.Background(), target).Args
	if !contains(lArgs, "-L") || !contains(lArgs, "myserver") {
		t.Errorf("args %v missing -L myserver when ServerName is set", lArgs)
	}
	if hasScrollbackPair(lArgs) {
		t.Errorf("args %v must NOT contain the scrollback pair -S - even with ServerName set", lArgs)
	}
}

// TestCaptureViewportCancel proves CaptureViewport threads ctx into the command via CommandContext: with
// an already-cancelled ctx, the command aborts and CaptureViewport returns a non-nil error. The seam is
// stubbed to a `sleep 5` so cancellation is the ONLY way it can return quickly — no real tmux needed.
func TestCaptureViewportCancel(t *testing.T) {
	orig := captureViewportCmd
	defer func() { captureViewportCmd = orig }()
	captureViewportCmd = func(ctx context.Context, target TmuxTarget) *exec.Cmd {
		return exec.CommandContext(ctx, "sleep", "5")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: CommandContext must abort the process on start

	if _, err := CaptureViewport(ctx, TmuxTarget{PaneID: "%3", Socket: "/tmp/sock"}); err == nil {
		t.Error("CaptureViewport with a cancelled ctx must return a non-nil error")
	}
}
