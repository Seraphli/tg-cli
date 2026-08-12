package helpers

import (
	"testing"

	"github.com/Seraphli/tg-cli/cmd/stores"
)

// TestTitleIsBusyCodex is the retargeted TestIsCodexBusyTitle: the same codex spinner-frame /
// truncated-name cases, now routed through the single classifier TitleIsBusy("codex", ...).
func TestTitleIsBusyCodex(t *testing.T) {
	// Codex busy: title starts with a braille spinner frame (U+2800–U+28FF).
	busy := []string{
		"⠋ project", "⠙ project", "⠹ project", "⠸ project", "⠼ project",
		"⠴ project", "⠦ project", "⠧ project", "⠇ project", "⠏ project",
		"⠙ ca-fix-codex-i...", // truncated busy title
	}
	for _, title := range busy {
		if !TitleIsBusy("codex", title) {
			t.Errorf("TitleIsBusy(codex, %q) = false, want true (busy)", title)
		}
	}
	// Codex idle: no braille prefix (plain or truncated dir name, empty).
	idle := []string{
		"ca-fix-codex-idle-detection", // full dir name
		"ca-messagedisplay-not...",    // truncated idle title (the original bug case)
		"tg-cli",
		"OBNote",
		"",
	}
	for _, title := range idle {
		if TitleIsBusy("codex", title) {
			t.Errorf("TitleIsBusy(codex, %q) = true, want false (idle)", title)
		}
	}
}

// TestTitleIsBusy exercises the single busy classifier directly across cc/codex/unknown backends, empty
// and whitespace-only titles, and the leading/trailing-whitespace cases the trim exists for. Fails if
// TitleIsBusy stops trimming or a prefix rule regresses.
func TestTitleIsBusy(t *testing.T) {
	cases := []struct {
		name    string
		backend string
		title   string
		want    bool
	}{
		{"cc idle", "cc", "✳ Job", false},
		{"cc busy", "cc", "· Working", true},
		{"codex busy braille", "codex", "⠙ project", true},
		{"codex idle name", "codex", "project", false},
		{"unknown backend never busy", "zsh", "· Working", false},
		{"empty string not busy", "cc", "", false},
		{"whitespace-only not busy (cc)", "cc", "   ", false},
		{"whitespace-only not busy (codex)", "codex", "   ", false},
		{"cc idle with surrounding whitespace", "cc", "  ✳ Job  ", false},
		{"codex busy with surrounding whitespace", "codex", "  ⠙ x  ", true},
	}
	for _, c := range cases {
		if got := TitleIsBusy(c.backend, c.title); got != c.want {
			t.Errorf("%s: TitleIsBusy(%q, %q) = %v, want %v", c.name, c.backend, c.title, got, c.want)
		}
	}
}

// TestStoreOrTitleBusyPi drives the pure helper storeOrTitleBusy with a REAL run-state store (no
// package var, no stub, no monkey-patch). It proves pi's store-first precedence POSITIVELY and that
// cc/codex ignore the store entirely.
func TestStoreOrTitleBusyPi(t *testing.T) {
	store := stores.NewHookRunningStateStore()
	target := "%1@/tmp/x"
	// A pi static title. pi is an unknown backend for TitleIsBusy (no cc/codex case), so the title
	// path always reads idle for pi — the store is the ONLY thing that can make a pi target busy.
	piTitle := "pi - proj"
	if TitleIsBusy("pi", piTitle) {
		t.Fatalf("precondition failed: TitleIsBusy(pi, %q) = true, want false (unknown backend → idle)", piTitle)
	}

	// 1. Before any store write the target is unknown (known==false), so pi falls through to the
	//    title path, which reads idle.
	if got := storeOrTitleBusy(store, "pi", target, piTitle); got != false {
		t.Errorf("pi before store write: storeOrTitleBusy = %v, want false (falls through to idle title)", got)
	}

	// 2. store-HIT precedence, proven POSITIVELY: the title alone reads idle (asserted above), so a
	//    true here can ONLY come from the store — this proves store-first precedence, not a miss fluke.
	store.SetRunning(target)
	if got := storeOrTitleBusy(store, "pi", target, piTitle); got != true {
		t.Errorf("pi with store RUNNING: storeOrTitleBusy = %v, want true (store hit, title would be idle)", got)
	}

	// 3. store flipped to idle → pi reads idle.
	store.SetIdle(target)
	if got := storeOrTitleBusy(store, "pi", target, piTitle); got != false {
		t.Errorf("pi with store IDLE: storeOrTitleBusy = %v, want false", got)
	}

	// 4. cc/codex ignore the store: with a RUNNING entry held for a DIFFERENT target, cc/codex verdicts
	//    must equal the direct TitleIsBusy result (store entry does NOT flip them). Idle titles chosen so
	//    the demonstration is meaningful: cc is idle only with the "✳" prefix; codex is idle for a plain
	//    (non-braille) title. Equality assertion is robust to the exact busy semantics regardless.
	target2 := "%2@/tmp/y"
	store.SetRunning(target2)
	ccIdleTitle := "✳ proj"
	codexIdleTitle := "proj"
	if got := storeOrTitleBusy(store, "cc", target2, ccIdleTitle); got != TitleIsBusy("cc", ccIdleTitle) {
		t.Errorf("cc ignores store: storeOrTitleBusy = %v, want %v (== TitleIsBusy)", got, TitleIsBusy("cc", ccIdleTitle))
	}
	if got := storeOrTitleBusy(store, "codex", target2, codexIdleTitle); got != TitleIsBusy("codex", codexIdleTitle) {
		t.Errorf("codex ignores store: storeOrTitleBusy = %v, want %v (== TitleIsBusy)", got, TitleIsBusy("codex", codexIdleTitle))
	}
}
