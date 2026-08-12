package notify

import (
	"strings"
	"testing"
)

// TestBuildNotificationTextInterrupted covers Round-4 Item 7: a streamed Message bubble flagged Interrupted
// (pi truncated it on a retryable error and is auto-retrying the SAME turn) renders the distinct
// "🔄 Interrupted — retrying…" header — NOT "💬 Message" and NOT "✅ Task Completed" (which would misreport a
// half-message as a complete answer). Interrupted takes precedence over Finalized. No regression to the other
// Message headers.
func TestBuildNotificationTextInterrupted(t *testing.T) {
	interrupted := BuildNotificationText(NotificationData{Event: "Message", Interrupted: true, ContextUsedPct: -1})
	if !strings.Contains(interrupted, "🔄") || !strings.Contains(interrupted, "Interrupted") || !strings.Contains(interrupted, "retrying") {
		t.Errorf("interrupted Message header missing the retry mark:\n%s", interrupted)
	}
	if strings.Contains(interrupted, "Task Completed") {
		t.Errorf("interrupted Message must NOT read as completed:\n%s", interrupted)
	}

	// Precedence: Interrupted wins over Finalized (an errored bubble is never a Stop-relabel target, but the
	// header must resolve to the interrupt mark if both are somehow set).
	both := BuildNotificationText(NotificationData{Event: "Message", Interrupted: true, Finalized: true, ContextUsedPct: -1})
	if !strings.Contains(both, "retrying") || strings.Contains(both, "Task Completed") {
		t.Errorf("Interrupted must take precedence over Finalized:\n%s", both)
	}

	// No regression: Finalized only -> ✅ Task Completed; neither -> 💬 Message. Neither carries the interrupt mark.
	final := BuildNotificationText(NotificationData{Event: "Message", Finalized: true, ContextUsedPct: -1})
	if !strings.Contains(final, "Task Completed") || strings.Contains(final, "retrying") {
		t.Errorf("finalized Message header regression:\n%s", final)
	}
	plain := BuildNotificationText(NotificationData{Event: "Message", ContextUsedPct: -1})
	if !strings.Contains(plain, "💬") || strings.Contains(plain, "retrying") {
		t.Errorf("plain Message header regression:\n%s", plain)
	}
}
