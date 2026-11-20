package helpers

import (
	"os/exec"
	"strings"
)

// psCliCommandForPID returns the CLI command line of the child process(es) of shellPID via
// `ps --ppid <shellPID>`. It serves the live path (GetPaneCLICommand, which fetches the pid via tmux then
// delegates here for a single target). Returns "" on empty pid or ps error.
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
