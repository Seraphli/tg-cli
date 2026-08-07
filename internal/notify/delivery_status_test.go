package notify

import (
	"strings"
	"testing"
)

func TestDeliveryStatusTag(t *testing.T) {
	cases := map[string]string{
		"":              "",
		"unconfirmed":   " ⚠️ delivery unconfirmed",
		"submit_failed": " ⚠️ submit failed (likely not executed)",
		"bogus":         "",
	}
	for in, want := range cases {
		if got := DeliveryStatusTag(in); got != want {
			t.Errorf("DeliveryStatusTag(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDeliveryStatusWarning(t *testing.T) {
	if got := DeliveryStatusWarning(""); got != "" {
		t.Errorf("DeliveryStatusWarning(\"\") = %q, want empty", got)
	}
	if got := DeliveryStatusWarning("unconfirmed"); !strings.Contains(got, "do NOT re-send") || !strings.Contains(got, "confirmation did not arrive") {
		t.Errorf("DeliveryStatusWarning(unconfirmed) = %q, missing expected text", got)
	}
	if got := DeliveryStatusWarning("submit_failed"); !strings.Contains(got, "submit FAILED") || !strings.Contains(got, "do NOT re-send") {
		t.Errorf("DeliveryStatusWarning(submit_failed) = %q, missing expected text", got)
	}
	if got := DeliveryStatusWarning("bogus"); got != "" {
		t.Errorf("DeliveryStatusWarning(bogus) = %q, want empty", got)
	}
}

// A SessionSend notification carrying a soft DeliveryStatus renders the short tag in its header;
// a normal ("") status renders no tag. Verified for both the rich body and the legacy body path.
func TestBuildNotificationText_SessionSendDeliveryStatus(t *testing.T) {
	base := NotificationData{Event: "SessionSend", SendFrom: "alice", Body: "hello", ContextUsedPct: -1}

	normal := BuildNotificationText(base)
	// f29: the visible status line is "CLI Send" (no " from alice"); the sender renders on a 👤 details line.
	if !strings.Contains(normal, "CLI Send") || strings.Contains(normal, "CLI Send from") {
		t.Fatalf("normal SessionSend visible line must be 'CLI Send' with no 'from' suffix: %q", normal)
	}
	if !strings.Contains(normal, "👤 alice") {
		t.Errorf("normal SessionSend missing 👤 sender line: %q", normal)
	}
	if strings.Contains(normal, "delivery unconfirmed") || strings.Contains(normal, "submit failed") {
		t.Errorf("normal SessionSend should carry no delivery tag: %q", normal)
	}

	unconf := base
	unconf.DeliveryStatus = "unconfirmed"
	if got := BuildNotificationText(unconf); !strings.Contains(got, "delivery unconfirmed") {
		t.Errorf("unconfirmed SessionSend missing tag: %q", got)
	}

	submit := base
	submit.DeliveryStatus = "submit_failed"
	if got := BuildNotificationText(submit); !strings.Contains(got, "submit failed (likely not executed)") {
		t.Errorf("submit_failed SessionSend missing tag: %q", got)
	}

	// Paginated (Page>0) header still carries the tag AND the 👤 sender line (the page-turn rebuild path
	// renders the same nd).
	paged := unconf
	paged.Page = 2
	paged.TotalPages = 3
	if got := BuildNotificationText(paged); !strings.Contains(got, "delivery unconfirmed") || !strings.Contains(got, "👤 alice") {
		t.Errorf("paginated unconfirmed SessionSend missing tag or 👤 sender line: %q", got)
	}
}

// f29 G: SessionSend visible line is the unified pen "🖊️ CLI Send" (+ delivery tag, NO "(silent)"); the
// sender renders on a 👤 details line, the normal/no-header type on a 🏷 details line, and there is NO
// "📂 " folder line when there is no project/CWD.
func TestBuildNotificationText_SessionSendSenderLine(t *testing.T) {
	base := NotificationData{Event: "SessionSend", SendFrom: "note", Body: "hi", ContextUsedPct: -1}

	got := BuildNotificationText(base)
	if !strings.Contains(got, "🖊️ CLI Send") || strings.Contains(got, " from ") || strings.Contains(got, "(silent)") {
		t.Errorf("visible line must be '🖊️ CLI Send' with no ' from '/'(silent)' suffix: %q", got)
	}
	if !strings.Contains(got, "👤 note") {
		t.Errorf("missing 👤 sender line: %q", got)
	}
	if !strings.Contains(got, "🏷 normal") {
		t.Errorf("normal SessionSend must render a '🏷 normal' type line: %q", got)
	}
	if strings.Contains(got, "📂 ") {
		t.Errorf("SessionSend has no project/CWD: must NOT render a 📂 folder line: %q", got)
	}

	silent := base
	silent.SendNoHeader = true
	silent.DeliveryStatus = "submit_failed"
	if g := BuildNotificationText(silent); !strings.Contains(g, "🖊️ CLI Send") || strings.Contains(g, "(silent)") || !strings.Contains(g, "submit failed (likely not executed)") || !strings.Contains(g, "👤 note") || !strings.Contains(g, "🏷 no-header") {
		t.Errorf("no-header+submit_failed SessionSend must show '🖊️ CLI Send' + tag + 👤 sender + '🏷 no-header': %q", g)
	}
}

// f29 F: position-based checkmark labels — a non-final Message bubble is "💬 Message"; the turn-FINAL
// (Finalized) streamed message and the Stop-direct-send are both "✅ Task Completed".
func TestBuildNotificationText_CheckmarkLabels(t *testing.T) {
	nonFinal := BuildNotificationText(NotificationData{Event: "Message", Body: "step", ContextUsedPct: -1})
	if !strings.Contains(nonFinal, "💬 Message") || strings.Contains(nonFinal, "Task Completed") {
		t.Errorf("non-final Message must be '💬 Message' (not Task Completed): %q", nonFinal)
	}
	finalMsg := BuildNotificationText(NotificationData{Event: "Message", Finalized: true, Body: "done", ContextUsedPct: -1})
	if !strings.Contains(finalMsg, "✅ Task Completed") || strings.Contains(finalMsg, "✅ Message") {
		t.Errorf("finalized Message must be '✅ Task Completed' (not '✅ Message'): %q", finalMsg)
	}
	stop := BuildNotificationText(NotificationData{Event: "Stop", Body: "done", ContextUsedPct: -1})
	if !strings.Contains(stop, "✅ Task Completed") {
		t.Errorf("Stop-direct-send must be '✅ Task Completed': %q", stop)
	}
}
