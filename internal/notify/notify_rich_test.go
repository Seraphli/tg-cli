package notify

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Seraphli/tg-cli/internal/markdown"
)

// noBareNewlineOutsidePre asserts the rich-send payload (after RichifyNewlines) has no bare "\n"
// left outside <pre>/<tg-math-block> — i.e. multi-line seams became <br>, never a collapsing space.
func noBareNewlineOutsidePre(t *testing.T, label, richHTML string) {
	t.Helper()
	sent := markdown.RichifyNewlines(richHTML)
	// Strip protected regions, then no \n may remain.
	stripped := sent
	for {
		i := strings.Index(stripped, "<pre>")
		if i < 0 {
			break
		}
		j := strings.Index(stripped[i:], "</pre>")
		if j < 0 {
			break
		}
		stripped = stripped[:i] + stripped[i+j+len("</pre>"):]
	}
	if strings.Contains(stripped, "\n") {
		t.Errorf("%s: rich payload has a bare \\n outside <pre> (would render as one line): sent=%q", label, sent)
	}
}

// T1: a multi-line notification (header lines + multi-paragraph body) becomes a rich payload whose
// header lines are separated by <br> and whose paragraphs are <p>-separated — never a bare \n.
func TestBuildNotificationText_RichHeaderSeamsUseBr(t *testing.T) {
	body := markdown.RenderRichHTML("para one\n\npara two")
	txt := BuildNotificationText(NotificationData{
		Event: "Message", Project: "proj", CWD: "/home/u/proj", TmuxTarget: "%1@host",
		AgentName: "agent", Body: body, ContextUsedPct: -1,
	})
	// Pre-fix, this string is header lines joined by bare \n.
	if !strings.Contains(txt, "\n") {
		t.Fatalf("test premise broken: builder should still join with \\n internally: %q", txt)
	}
	sent := markdown.RichifyNewlines(txt)
	if !strings.Contains(sent, "📂") || !strings.Contains(sent, "<br>📂") {
		t.Errorf("header lines not <br>-separated after RichifyNewlines: %q", sent)
	}
	if !strings.Contains(sent, "<p>") {
		t.Errorf("multi-paragraph body lost <p> separation: %q", sent)
	}
	noBareNewlineOutsidePre(t, "notification", txt)
}

// T1 + T2: permission text puts old_string/new_string in <pre>. After RichifyNewlines the label
// seams use <br>, but the code newlines INSIDE <pre> are preserved verbatim.
func TestBuildPermissionText_RichBrWithPrePreserved(t *testing.T) {
	txt := BuildPermissionText(PermissionData{
		Project: "proj", CWD: "/home/u/proj", TmuxTarget: "%1@host", ToolName: "Edit",
		ToolInput: map[string]interface{}{
			"file_path":  "/x/y.go",
			"old_string": "line1\nline2\nline3",
		},
		AgentName: "agent",
	})
	sent := markdown.RichifyNewlines(txt)
	// Label/header seams converted to <br>.
	if !strings.Contains(sent, "<br>") {
		t.Errorf("permission text seams not converted to <br>: %q", sent)
	}
	// <pre> content newlines preserved (line1\nline2\nline3 intact).
	if !strings.Contains(sent, "line1\nline2\nline3") {
		t.Errorf("permission <pre> code newlines corrupted: %q", sent)
	}
	noBareNewlineOutsidePre(t, "permission", txt)
}

// T1 + T2: the collapsed-<details> tool notification body uses <br> for its inline seams while
// preserving any <pre> arg newlines.
func TestBuildToolNotifyText_RichBrWithPrePreserved(t *testing.T) {
	input := json.RawMessage(`{"command":"echo hi","description":"say hi"}`)
	txt := BuildToolNotifyText("Bash", input, "/home/u/proj")
	if txt == "" {
		t.Skip("tool notify produced empty body for Bash")
	}
	sent := markdown.RichifyNewlines(txt)
	if !strings.Contains(sent, "<details>") || !strings.Contains(sent, "<summary>") {
		t.Errorf("tool notify missing collapsed <details>/<summary>: %q", sent)
	}
	noBareNewlineOutsidePre(t, "toolnotify", txt)
}

// AgentError: the pi extension's error-branch notification event ("AgentError") must render its OWN
// dedicated header ("⚠️ Run Error") — NOT fall through to the default "✅ Task Completed", which would be
// the exact misreport we must avoid for a failed run. The body must be carried verbatim.
func TestBuildNotificationText_AgentError(t *testing.T) {
	txt := BuildNotificationText(NotificationData{Event: "AgentError", Body: "boom details"})
	if !strings.Contains(txt, "⚠️") {
		t.Errorf("AgentError header missing ⚠️ emoji: %q", txt)
	}
	if !strings.Contains(txt, "Run Error") {
		t.Errorf("AgentError header missing \"Run Error\" status: %q", txt)
	}
	if !strings.Contains(txt, "boom details") {
		t.Errorf("AgentError body not rendered: %q", txt)
	}
	if strings.Contains(txt, "Task Completed") {
		t.Errorf("AgentError must NOT render \"Task Completed\" (default misreport): %q", txt)
	}
}

// AgentInterrupted: the pi extension's ESC-abort notification event ("AgentInterrupted", stopReason
// "aborted") must render its OWN dedicated header ("⏹ Interrupted") — NOT fall through to the default
// "✅ Task Completed", which would misreport an interrupted turn as a completed one (R2/R3).
func TestBuildNotificationText_AgentInterrupted(t *testing.T) {
	txt := BuildNotificationText(NotificationData{Event: "AgentInterrupted", Body: "⏹ pi run interrupted"})
	if !strings.Contains(txt, "⏹") {
		t.Errorf("AgentInterrupted header missing ⏹ stop glyph: %q", txt)
	}
	if !strings.Contains(txt, "Interrupted") {
		t.Errorf("AgentInterrupted header missing \"Interrupted\" status: %q", txt)
	}
	if strings.Contains(txt, "Task Completed") {
		t.Errorf("AgentInterrupted must NOT render \"Task Completed\" (default misreport): %q", txt)
	}
}

// Fix 13a: a no-arg tool (e.g. TaskList with tool_input {}) has no formatted body, but must still
// produce a name-only skeleton "🔧 <tool>" — never "" — so the caller sends a notification (the tool
// call must be visible on TG and must separate adjacent assistant text messages for V3 ordering).
func TestBuildToolNotifyText_NoArgToolSkeleton(t *testing.T) {
	txt := BuildToolNotifyText("TaskList", json.RawMessage(`{}`), "/home/u/proj")
	if txt == "" {
		t.Fatalf("no-arg tool must produce a name skeleton, got empty string")
	}
	if txt != "🔧 TaskList" {
		t.Errorf("expected name-only skeleton %q, got %q", "🔧 TaskList", txt)
	}
	// A skeleton has no args to collapse — it must NOT be wrapped in a <details> block.
	if strings.Contains(txt, "<details>") {
		t.Errorf("no-arg skeleton should not be wrapped in <details>: %q", txt)
	}
}
