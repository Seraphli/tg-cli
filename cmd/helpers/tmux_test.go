package helpers

import "testing"

// TestDetectBackendPi verifies that a pane whose current command is "pi"
// is classified as the "pi" backend. The cliCommand closure is unused for
// non-node panes, so a stub returning "" is sufficient.
func TestDetectBackendPi(t *testing.T) {
	if got := detectBackend("pi", func() string { return "" }); got != "pi" {
		t.Fatalf("detectBackend(pi) = %q, want pi", got)
	}
}
