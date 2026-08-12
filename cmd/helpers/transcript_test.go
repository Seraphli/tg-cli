package helpers

import (
	"encoding/json"
	"os"
	"path/filepath"
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

// --- f29 round-13: @ forward round determination uses origin.kind for CC human turns ---
// NOTHING is removed from output; only whether a CC user entry OPENS A ROUND changes. A CC user entry
// opens a round iff origin.kind=="human"; a non-human CC user entry (nudge, interrupt) is still shown but
// does not open a round. codex (no origin field) is unchanged. The probe DIFFERS per function because
// ReadContextBlock uses noTools=false (a Bash tool_use survives) while ReadLastNRounds uses noTools=true
// (a pure Bash tool_use is filtered out), so a tool fixture cannot discriminate on ReadLastNRounds.

func mustJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "transcript-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()
	return f.Name()
}

// --- CC record builders (origin / isMeta are TOP-LEVEL fields, siblings of message) ---
func ccHumanStr(text string) string {
	return mustJSON(map[string]interface{}{
		"type": "user", "origin": map[string]string{"kind": "human"},
		"message": map[string]interface{}{"content": text},
	})
}
func ccHumanArr(text string) string {
	return mustJSON(map[string]interface{}{
		"type": "user", "origin": map[string]string{"kind": "human"},
		"message": map[string]interface{}{"content": []map[string]string{{"type": "text", "text": text}}},
	})
}
func ccUserOriginStr(kind, text string) string {
	return mustJSON(map[string]interface{}{
		"type": "user", "origin": map[string]string{"kind": kind},
		"message": map[string]interface{}{"content": text},
	})
}
func ccMetaStr(text string) string {
	return mustJSON(map[string]interface{}{
		"type": "user", "isMeta": true,
		"message": map[string]interface{}{"content": text},
	})
}
func ccMetaArr(text string) string {
	return mustJSON(map[string]interface{}{
		"type": "user", "isMeta": true,
		"message": map[string]interface{}{"content": []map[string]string{{"type": "text", "text": text}}},
	})
}
func ccUserArrNoOrigin(text string) string {
	return mustJSON(map[string]interface{}{
		"type":    "user",
		"message": map[string]interface{}{"content": []map[string]string{{"type": "text", "text": text}}},
	})
}
func ccAsstText(text string) string {
	return mustJSON(map[string]interface{}{
		"type":    "assistant",
		"message": map[string]interface{}{"content": []map[string]string{{"type": "text", "text": text}}},
	})
}
func ccAsstBash(cmd string) string {
	return mustJSON(map[string]interface{}{
		"type": "assistant",
		"message": map[string]interface{}{"content": []map[string]interface{}{
			{"type": "tool_use", "name": "Bash", "input": map[string]string{"command": cmd}},
		}},
	})
}

// user record whose array content is [text + tool_result]; origin optional (site :81, ReadLastNRounds only)
func ccUserTextToolResult(originKind, text string) string {
	m := map[string]interface{}{
		"type": "user",
		"message": map[string]interface{}{"content": []map[string]interface{}{
			{"type": "text", "text": text},
			{"type": "tool_result", "content": "toolout"},
		}},
	}
	if originKind != "" {
		m["origin"] = map[string]string{"kind": originKind}
	}
	return mustJSON(m)
}

func codexUser(text string) string {
	return mustJSON(map[string]interface{}{
		"type": "response_item",
		"payload": map[string]interface{}{"type": "message", "role": "user",
			"content": []map[string]string{{"type": "input_text", "text": text}}},
	})
}
func codexAsst(text string) string {
	return mustJSON(map[string]interface{}{
		"type": "response_item",
		"payload": map[string]interface{}{"type": "message", "role": "assistant",
			"content": []map[string]string{{"type": "output_text", "text": text}}},
	})
}

const (
	selfRecov1   = "[Your previous response had no visible output. Please continue and produce a user-visible response.]"
	selfRecov2   = "The previous response failed to produce a valid tool call..."
	selfRecov3   = "Your tool call was malformed and could not be parsed..."
	selfRecov4   = "The PermissionDenied hook indicated you may retry this tool call."
	interruptTxt = "[Request interrupted by user for tool use]"
)

type nhCase struct {
	name  string
	vtext string
	vline string
}

func nonHumanCases() []nhCase {
	return []nhCase{
		{"interrupt-array", interruptTxt, ccUserArrNoOrigin(interruptTxt)},                                               // site :92
		{"nonhuman-origin-string", "system routed notice", ccUserOriginStr("task-notification", "system routed notice")}, // site :55
		{"ismeta-string", "meta please continue str", ccMetaStr("meta please continue str")},                             // site :55
		{"ismeta-array", "meta please continue arr", ccMetaArr("meta please continue arr")},                              // site :92
		{"self-recovery-1", selfRecov1, ccMetaArr(selfRecov1)},
		{"self-recovery-2", selfRecov2, ccMetaArr(selfRecov2)},
		{"self-recovery-3", selfRecov3, ccMetaArr(selfRecov3)},
		{"self-recovery-4", selfRecov4, ccMetaArr(selfRecov4)},
	}
}

// (i) ReadContextBlock probe (noTools=false): a Bash tool_use survives, so a nudge that wrongly opened a
// round would evict the tool round at rounds=1. Non-human V must be SHOWN and must NOT open a round.
func TestReadContextBlock_NonHumanUserShownNoRound(t *testing.T) {
	for _, c := range nonHumanCases() {
		t.Run(c.name, func(t *testing.T) {
			path := writeTranscript(t,
				ccHumanStr("H1"),
				ccAsstBash("echo hi"),
				c.vline,
				ccAsstText("done"),
			)
			out, err := ReadContextBlock(path, 1, 0, "cc", "e2e-cli", "user")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, "🔧 Bash") {
				t.Fatalf("tool line evicted -> V wrongly opened a round: %q", out)
			}
			if !strings.Contains(out, c.vtext) {
				t.Fatalf("non-human user text must still be shown (no drop): %q", out)
			}
			if !strings.Contains(out, "H1") {
				t.Fatalf("H1 should remain in the single round: %q", out)
			}
		})
	}
}

// (i) ReadLastNRounds probe (noTools=true): a Bash tool_use is filtered out, so use visible assistant text.
// Pre-fix V opens a round and n=1 evicts the H1/A1 round; post-fix V is non-human so H1/A1 are RETAINED.
func TestReadLastNRounds_NonHumanUserShownNoRound(t *testing.T) {
	for _, c := range nonHumanCases() {
		t.Run(c.name, func(t *testing.T) {
			path := writeTranscript(t,
				ccHumanStr("H1"),
				ccAsstText("A1"),
				c.vline,
				ccAsstText("A2"),
			)
			rounds, err := ReadLastNRounds(path, 1, "cc")
			if err != nil {
				t.Fatal(err)
			}
			if len(rounds) != 1 {
				t.Fatalf("V must not split; expected 1 round, got %d: %+v", len(rounds), rounds)
			}
			all := strings.Join(append(append([]string{}, rounds[0].UserTexts...), rounds[0].AssistantTexts...), "|")
			for _, want := range []string{"H1", "A1", c.vtext, "A2"} {
				if !strings.Contains(all, want) {
					t.Fatalf("expected %q retained in single round, got %q", want, all)
				}
			}
		})
	}
}

// (ii) CC human opens a round — ReadContextBlock, string and array human prompts.
func TestReadContextBlock_HumanOpensRound(t *testing.T) {
	for _, h := range []struct{ name, line string }{
		{"human-string", ccHumanStr("H2")},
		{"human-array", ccHumanArr("H2")},
	} {
		t.Run(h.name, func(t *testing.T) {
			path := writeTranscript(t, ccHumanStr("H1"), ccAsstText("A1"), h.line, ccAsstText("A2"))
			out, err := ReadContextBlock(path, 1, 0, "cc", "e2e-cli", "user")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, "H2") || !strings.Contains(out, "A2") {
				t.Fatalf("last (human) round should be present: %q", out)
			}
			if strings.Contains(out, "H1") || strings.Contains(out, "A1") {
				t.Fatalf("human H2 should open a new round, evicting round 1: %q", out)
			}
		})
	}
}

// (ii) CC human opens a round — ReadLastNRounds, string and array human prompts.
func TestReadLastNRounds_HumanOpensRound(t *testing.T) {
	for _, h := range []struct{ name, line string }{
		{"human-string", ccHumanStr("H2")},
		{"human-array", ccHumanArr("H2")},
	} {
		t.Run(h.name, func(t *testing.T) {
			path := writeTranscript(t, ccHumanStr("H1"), ccAsstText("A1"), h.line, ccAsstText("A2"))
			rounds, err := ReadLastNRounds(path, 1, "cc")
			if err != nil {
				t.Fatal(err)
			}
			if len(rounds) != 1 {
				t.Fatalf("expected 1 round kept, got %d", len(rounds))
			}
			if u := strings.Join(rounds[0].UserTexts, "|"); u != "H2" {
				t.Fatalf("expected UserTexts=[H2], got %q", u)
			}
			if a := strings.Join(rounds[0].AssistantTexts, "|"); a != "A2" {
				t.Fatalf("expected AssistantTexts=[A2], got %q", a)
			}
		})
	}
}

// (iii) noTools text+tool_result branch (:81), ReadLastNRounds only, correct on BOTH sides.
func TestReadLastNRounds_ToolResultBranch(t *testing.T) {
	t.Run("human-opens", func(t *testing.T) {
		path := writeTranscript(t,
			ccHumanStr("H1"), ccAsstText("A1"),
			ccUserTextToolResult("human", "TT"), ccAsstText("A2"),
		)
		rounds, err := ReadLastNRounds(path, 1, "cc")
		if err != nil {
			t.Fatal(err)
		}
		if len(rounds) != 1 {
			t.Fatalf("expected 1 round, got %d", len(rounds))
		}
		u := strings.Join(rounds[0].UserTexts, "|")
		if !strings.Contains(u, "TT") {
			t.Fatalf(":81 text must appear in UserTexts: %q", u)
		}
		if strings.Contains(u, "H1") {
			t.Fatalf("human V should open a round, evicting H1: %q", u)
		}
	})
	t.Run("nonhuman-noround", func(t *testing.T) {
		path := writeTranscript(t,
			ccHumanStr("H1"), ccAsstText("A1"),
			ccUserTextToolResult("", "TT"), ccAsstText("A2"),
		)
		rounds, err := ReadLastNRounds(path, 1, "cc")
		if err != nil {
			t.Fatal(err)
		}
		if len(rounds) != 1 {
			t.Fatalf("expected 1 round, got %d", len(rounds))
		}
		u := strings.Join(rounds[0].UserTexts, "|")
		if !strings.Contains(u, "TT") {
			t.Fatalf(":81 text must appear in UserTexts: %q", u)
		}
		if !strings.Contains(u, "H1") {
			t.Fatalf("non-human V must not open a round; H1 retained: %q", u)
		}
	})
}

// (iv) codex regression — output must be byte-identical to before (the ccBackend gate must not affect codex;
// without it, no codex user would open a round, the whole transcript would collapse to one round, and
// rounds=1 would leak round 1).
func TestCodexRoundsUnchanged(t *testing.T) {
	path := writeTranscript(t,
		codexUser("cx-user-1"), codexAsst("cx-asst-1"),
		codexUser("cx-user-2"), codexAsst("cx-asst-2"),
	)
	out, err := ReadContextBlock(path, 1, 0, "codex", "e2e-cli", "user")
	if err != nil {
		t.Fatal(err)
	}
	want := "[user → e2e-cli]: cx-user-2\n[e2e-cli → user]: cx-asst-2"
	if out != want {
		t.Fatalf("codex ReadContextBlock changed:\n got: %q\nwant: %q", out, want)
	}
	rounds, err := ReadLastNRounds(path, 1, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 1 ||
		strings.Join(rounds[0].UserTexts, "|") != "cx-user-2" ||
		strings.Join(rounds[0].AssistantTexts, "|") != "cx-asst-2" {
		t.Fatalf("codex ReadLastNRounds changed: %+v", rounds)
	}
}

// TC-transcript: ParsePiTranscript on a pi v3 JSONL fixture must emit the final assistant
// text ("FINAL ANSWER") and EXCLUDE the thinking block content ("SECRET").
func TestParsePiTranscript(t *testing.T) {
	// pi v3 entry-wrapper lines: session header, user message, assistant message whose
	// content is [thinking(SECRET), text(FINAL ANSWER)] with stopReason "stop".
	fixture := `{"type":"session","version":3,"id":"sess-uuid","timestamp":"2026-08-08T00:00:00.000Z","cwd":"/x"}
{"type":"message","id":"u1","parentId":null,"timestamp":"2026-08-08T00:00:01.000Z","message":{"role":"user","content":"hello"}}
{"type":"message","id":"a1","parentId":"u1","timestamp":"2026-08-08T00:00:02.000Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"SECRET"},{"type":"text","text":"FINAL ANSWER"}],"provider":"newapi","model":"deepseek-v4-flash-free","stopReason":"stop"}}
`
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	noop := func(string, map[string]interface{}) string { return "" }
	entries := ParsePiTranscript(f, false, nil, noop)
	// Find the LAST assistant entry.
	var last *TranscriptLogEntry
	for i := range entries {
		if entries[i].Type == "assistant" {
			last = &entries[i]
		}
	}
	if last == nil {
		t.Fatalf("no assistant entry parsed from fixture: %+v", entries)
	}
	if last.Text != "FINAL ANSWER" {
		t.Fatalf("expected assistant Text %q, got %q", "FINAL ANSWER", last.Text)
	}
	if strings.Contains(last.Text, "SECRET") {
		t.Fatalf("thinking content must be excluded, but Text contains SECRET: %q", last.Text)
	}
}

// TestParsePiTranscriptToolDetail covers Round-2 Item B: ParsePiTranscript must now CALL the formatToolDetail
// callback (it previously took it but never invoked it, hardcoding the raw arguments JSON). With extractToolParam
// as the formatter, a pi bash toolCall renders the command string, and entry.Tool keeps pi's lowercase name (O2).
// RED on the pre-fix build (ToolDetail is the raw `{"command":...}` JSON).
func TestParsePiTranscriptToolDetail(t *testing.T) {
	fixture := `{"type":"session","version":3,"id":"sess","timestamp":"2026-08-08T00:00:00.000Z","cwd":"/x"}
{"type":"message","id":"a1","timestamp":"2026-08-08T00:00:02.000Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"call_1","name":"bash","arguments":{"command":"echo hello_world"}}],"stopReason":"toolUse"}}
`
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	formatToolDetail := func(name string, input map[string]interface{}) string { return extractToolParam(name, input) }
	entries := ParsePiTranscript(f, false, nil, formatToolDetail)
	var tool *TranscriptLogEntry
	for i := range entries {
		if entries[i].Tool != "" {
			tool = &entries[i]
			break
		}
	}
	if tool == nil {
		t.Fatalf("no tool entry parsed from the pi transcript: %+v", entries)
	}
	if tool.Tool != "bash" {
		t.Errorf("entry.Tool = %q, want pi lowercase %q (O2)", tool.Tool, "bash")
	}
	// The detail must be the rendered command (via formatToolDetail + NormalizePiToolName), NOT the raw args JSON.
	if tool.ToolDetail != "echo hello_world" {
		t.Errorf("entry.ToolDetail = %q, want the command %q (pre-fix dumps the raw arguments JSON)", tool.ToolDetail, "echo hello_world")
	}
}
