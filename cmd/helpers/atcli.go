package helpers

import (
	"fmt"
	"os"
	"path/filepath"
)

// atCLI returns the command head and the " --port N" suffix used when rendering @ channel
// reply/end CLI instructions for a target or initiator session.
//
// Default production config (configDir == "" or ~/.tg-cli) → ("tg-cli", ""): a session whose
// bare `tg-cli` already resolves to this (default) bot. Non-default config-dir (e.g. the E2E
// test bot at ~/.tg-cli-test) → ("tg-cli --config-dir <configDir>", " --port <port>"), so the
// instructed session reaches THIS bot instead of the default-config (production) bot. The
// non-default check mirrors cmd/setup.go. configDir is rendered unquoted, consistent with the
// existing hook command in cmd/setup.go (assumes space-free config paths, true for prod/test).
func atCLI(configDir string, port int) (head string, portFlag string) {
	home, _ := os.UserHomeDir()
	if configDir == "" || configDir == filepath.Join(home, ".tg-cli") {
		return "tg-cli", ""
	}
	return "tg-cli --config-dir " + configDir, fmt.Sprintf(" --port %d", port)
}

// AtReplyCommand renders the full `session at reply` CLI command embedded in @ channel reply
// instructions. Default config → byte-for-byte the legacy bare command
// (`tg-cli session at reply <from> <to> --text "your message"`); non-default config →
// `tg-cli --config-dir <dir> session at reply <from> <to> --port <port> --text "your message"`.
func AtReplyCommand(configDir string, port int, from, to string) string {
	head, portFlag := atCLI(configDir, port)
	return fmt.Sprintf("%s session at reply %s %s%s --text \"your message\"", head, from, to, portFlag)
}

// AtEndCommand renders the full `session at end` CLI command. Default config →
// `tg-cli session at end <from> <to>`; non-default config →
// `tg-cli --config-dir <dir> session at end <from> <to> --port <port>`.
func AtEndCommand(configDir string, port int, from, to string) string {
	head, portFlag := atCLI(configDir, port)
	return fmt.Sprintf("%s session at end %s %s%s", head, from, to, portFlag)
}
