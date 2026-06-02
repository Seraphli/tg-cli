package helpers

import (
	"strings"
	"testing"
)

func TestFormatToolLineTargetMax(t *testing.T) {
	// default (≤0 → 40): long command is truncated with …
	def := FormatToolLine("e2e-cli", "Bash", "echo a-very-long-command-string-here", 0)
	if !strings.Contains(def, "…") {
		t.Fatalf("default targetMax should truncate long param: %q", def)
	}
	// tiny targetMax → no-param form (paramBudget ≤ 0)
	tiny := FormatToolLine("e2e-cli", "Bash", "echo whatever", 1)
	if strings.Contains(tiny, "(") {
		t.Fatalf("tiny targetMax should drop the param: %q", tiny)
	}
}
