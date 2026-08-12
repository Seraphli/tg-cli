package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/internal/config"
)

// Ticker paragraph-alignment behaviour tests (F1/F2/F5). They exercise the flushSession snapshot/commit (p1/p3)
// on the gated-worker/nil-Bot harness from stream_sendbelow_test.go and assert ONLY on EXISTING observables
// (the flushSession return value, readPositioned, and Dirty) so each is a genuine RED on the pre-change build.

// F1: a non-final TICKER flush renders only up to the last "\n\n"; the partial tail after the boundary is NOT
// rendered. Proven via PositionedChunks: with a small richMaxRunes the aligned body ("AAAMARK short paragraph\n\n")
// is a single chunk, whereas the FULL body (aligned + a long no-boundary tail) would spill to several. RED when
// the whole alignment block is reverted (the ticker renders the full body -> more than one chunk).
func TestTickerFlushAlignsToBoundary(t *testing.T) {
	bs := newSendBelowBS(t)
	if err := os.WriteFile(filepath.Join(config.GetConfigDir(), "config.json"), []byte(`{"richMaxRunes":600}`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	sid := "align-f1"
	gateWorker(t, bs, sid)
	// One complete paragraph + boundary, then a long partial tail with NO trailing "\n\n".
	body := "AAAMARK short paragraph\n\n" + strings.Repeat("x", 3000)
	bs.Streams.AppendDelta(sid, stores.StreamMeta{MessageID: "m1", ChatID: 1}, 0, body, false)
	if !flushSession(bs, sid, false, false, flushTry) {
		t.Fatal("F1: a bubble past a paragraph boundary must enqueue")
	}
	if got := readPositioned(bs, sid, "m1"); got != 1 {
		t.Fatalf("F1: the aligned body must be exactly 1 chunk (the long tail after the \\n\\n truncated), got %d", got)
	}
}

// F2: a non-final TICKER flush with NO "\n\n" is skipped — no enqueue, no commit, Dirty preserved for retry.
// RED when the whole alignment block is reverted (the ticker renders the partial text -> enqueues, returns true,
// clears Dirty).
func TestTickerFlushNoBoundarySkips(t *testing.T) {
	bs := newSendBelowBS(t)
	sid := "align-f2"
	gateWorker(t, bs, sid)
	bs.Streams.AppendDelta(sid, stores.StreamMeta{MessageID: "m1", ChatID: 1}, 0, "no blank line here just running words", false)
	if flushSession(bs, sid, false, false, flushTry) {
		t.Fatal("F2: a ticker flush with no paragraph boundary must SKIP (return false, no enqueue)")
	}
	if got := readPositioned(bs, sid, "m1"); got != 0 {
		t.Fatalf("F2: a no-boundary skip must not commit PositionedChunks, got %d", got)
	}
	ss := bs.Streams.Session(sid)
	ss.DataMu.Lock()
	dirty := ss.Msgs["m1"].Dirty
	ss.DataMu.Unlock()
	if !dirty {
		t.Fatal("F2: Dirty must be preserved so a later delta/tick retries")
	}
}

// F5: after a NON-ticker full flush renders a mid-paragraph body T2 (committing RenderedLen=len(T2)), the stream
// grows with NO new "\n\n"; the next TICKER flush computes an aligned body ending at the OLDER boundary (shorter
// than T2) and MUST skip — no enqueue, PositionedChunks unchanged, Dirty preserved — so the bubble never shrinks.
// RED when ONLY the F5 guard is reverted (the ticker enqueues the shorter aligned body -> returns true, clears
// Dirty).
func TestTickerFlushNeverShrinks(t *testing.T) {
	bs := newSendBelowBS(t)
	sid := "align-f5"
	gateWorker(t, bs, sid)
	// T2: one complete paragraph + a mid-paragraph tail (no trailing "\n\n").
	bs.Streams.AppendDelta(sid, stores.StreamMeta{MessageID: "m1", ChatID: 1}, 0, "P1 alpha\n\nMID mid-paragraph tail", false)
	if !flushSession(bs, sid, false, false, flushAsync) {
		t.Fatal("F5: the non-ticker full flush must enqueue T2")
	}
	pBefore := readPositioned(bs, sid, "m1")
	if pBefore < 1 {
		t.Fatalf("F5: T2 must position at least 1 chunk, got %d", pBefore)
	}
	// Grow the SAME last paragraph with NO new "\n\n".
	handled, dropped, _ := bs.Streams.AppendExisting(sid, "m1", "", 1, " more still no newline", false)
	if !handled || dropped {
		t.Fatalf("F5: the growth continuation must append, got handled=%v dropped=%v", handled, dropped)
	}
	// The ticker aligned body = "P1 alpha\n\n" (the older boundary) — shorter than T2 -> F5 skip.
	if flushSession(bs, sid, false, false, flushTry) {
		t.Fatal("F5: the ticker must SKIP (return false) when the aligned body would be shorter than the last rendered body")
	}
	if pAfter := readPositioned(bs, sid, "m1"); pAfter != pBefore {
		t.Fatalf("F5: PositionedChunks must stay %d (no shrink render), got %d", pBefore, pAfter)
	}
	ss := bs.Streams.Session(sid)
	ss.DataMu.Lock()
	dirty := ss.Msgs["m1"].Dirty
	ss.DataMu.Unlock()
	if !dirty {
		t.Fatal("F5: Dirty must be preserved so a later boundary retries")
	}
}
