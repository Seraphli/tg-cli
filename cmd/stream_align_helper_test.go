package cmd

import (
	"testing"

	"github.com/Seraphli/tg-cli/cmd/stores"
)

// TestAlignToParagraph covers the F1 truncation helper across the boss's required edge set — no "\n\n", trailing
// "\n\n", multiple "\n\n", "\n\n\n", text ending exactly at "\n\n" — plus the leading-blank-line case that
// note3's §7 ruling added (the helper returns "\n\n" ok=true; the flushSession F2 TrimSpace clause is what skips
// rendering it). "up to AND including the last \n\n" means the returned prefix ends with the boundary bytes.
func TestAlignToParagraph(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"no boundary", "abc", "", false},
		{"trailing boundary", "a\n\nb\n\n", "a\n\nb\n\n", true},
		{"multiple boundaries", "a\n\nb\n\nc", "a\n\nb\n\n", true},
		{"triple newline", "\n\n\n", "\n\n\n", true},
		{"ends exactly at boundary", "a\n\n", "a\n\n", true},
		{"leading blank line", "\n\nabc", "\n\n", true},
	}
	for _, c := range cases {
		got, ok := alignToParagraph(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: alignToParagraph(%q) = (%q, %v), want (%q, %v)", c.name, c.in, got, ok, c.want, c.ok)
		}
	}
}

// readRenderedLen reads an entry's RenderedLen (the F5 high-water) under DataMu (-1 if the entry is gone).
func readRenderedLen(bs *BotState, sid, mid string) int {
	ss := bs.Streams.Session(sid)
	ss.DataMu.Lock()
	defer ss.DataMu.Unlock()
	if e := ss.Msgs[mid]; e != nil {
		return e.RenderedLen
	}
	return -1
}

// TestNonTickerFlushRendersFull (F4): a non-ticker flush (flushAsync) renders the FULL text even mid-paragraph —
// the alignment block is gated on mode==flushTry. The committed RenderedLen equals len(full text). RED when the
// mode-gate is reverted (align all modes): flushAsync would truncate to "P1 alpha\n\n" and RenderedLen would be
// shorter than len(full).
func TestNonTickerFlushRendersFull(t *testing.T) {
	bs := newSendBelowBS(t)
	sid := "align-f4"
	gateWorker(t, bs, sid)
	full := "P1 alpha\n\nmid partial no newline"
	bs.Streams.AppendDelta(sid, stores.StreamMeta{MessageID: "m1", ChatID: 1}, 0, full, false)
	if !flushSession(bs, sid, false, false, flushAsync) {
		t.Fatal("F4: a non-ticker flush must enqueue a render op")
	}
	if got := readRenderedLen(bs, sid, "m1"); got != len(full) {
		t.Fatalf("F4: a non-ticker flush must render the FULL mid-paragraph text (RenderedLen=%d, want %d)", got, len(full))
	}
}
