package helpers

import "testing"

// TestCodexTurnError reads rollout files built from real codex event lines (key order and shapes byte-exact,
// invented ids, times and text). An error turn in its real order ends with token_count lines, the task_complete
// that carries the error, then thread_settings_applied lines; a message sent right after the error starts a
// new turn, whose task_started is written before the turn's hook fires.
func TestCodexTurnError(t *testing.T) {
	for _, c := range []struct{ name, file, want string }{
		{"error turn, thread settings after it", "error-turn.jsonl", "The usage limit of this test account is reached."},
		{"normal turn", "normal-turn.jsonl", ""},
		{"new turn started after an error turn", "error-then-new-turn.jsonl", ""},
	} {
		got, err := CodexTurnError("../../tests/fixtures/codex-rollout/" + c.file)
		if err != nil || got != c.want {
			t.Errorf("%s: CodexTurnError(%s) = (%q, %v), want (%q, nil)", c.name, c.file, got, err, c.want)
		}
	}
}
