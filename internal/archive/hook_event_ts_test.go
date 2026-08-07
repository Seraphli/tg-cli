package archive

import (
	"testing"
	"time"
)

// TestRecordHookEventAt_OrderByTsIsArrivalOrder (S14a, rev 14 BLOCKER1 + rev 15/16 MINOR): RecordHookEventAt
// stamps ts from the ingress arrivedAt with a FIXED-WIDTH 9-digit-fraction UTC layout so TEXT ORDER BY ts is
// lexicographically == chronologically ordered. Two events in the SAME millisecond get distinct ns ts, and
// ORDER BY ts yields their arrival order even when they are INSERTED in reverse order (event_id would then be
// reverse — ts is the authoritative arrival key).
func TestRecordHookEventAt_OrderByTsIsArrivalOrder(t *testing.T) {
	a, _ := newTestArchive(t)
	base := time.Date(2026, 7, 16, 1, 2, 3, 0, time.UTC)
	// Two events in the same millisecond, distinguished only by nanoseconds. INSERT them in REVERSE arrival
	// order so event_id (AUTOINCREMENT) is reverse — only ts ORDER BY can recover arrival order.
	early := base.Add(100 * time.Nanosecond) // arrived FIRST
	late := base.Add(900 * time.Nanosecond)  // arrived SECOND (same ms)
	if err := a.RecordHookEventAt("PreToolUse", "sess-1", "m-late", "t", "p", []byte(`{}`), late); err != nil {
		t.Fatalf("RecordHookEventAt late: %v", err)
	}
	if err := a.RecordHookEventAt("MessageDisplay", "sess-1", "m-early", "t", "p", []byte(`{}`), early); err != nil {
		t.Fatalf("RecordHookEventAt early: %v", err)
	}
	// ORDER BY ts must give arrival order: early (m-early) then late (m-late), NOT the insert/event_id order.
	rows, err := a.db.Query(`SELECT message_id FROM hook_events WHERE session_id='sess-1' ORDER BY ts`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var order []string
	for rows.Next() {
		var mid string
		if err := rows.Scan(&mid); err != nil {
			t.Fatalf("scan: %v", err)
		}
		order = append(order, mid)
	}
	if len(order) != 2 || order[0] != "m-early" || order[1] != "m-late" {
		t.Fatalf("ORDER BY ts must yield arrival order [m-early m-late], got %v", order)
	}
	// The two ts values must be DISTINCT (fixed-width ns, not truncated to ms).
	var distinct int
	if err := a.db.QueryRow(`SELECT COUNT(DISTINCT ts) FROM hook_events WHERE session_id='sess-1'`).Scan(&distinct); err != nil {
		t.Fatalf("count distinct ts: %v", err)
	}
	if distinct != 2 {
		t.Fatalf("same-ms events must get distinct ns ts, got %d distinct", distinct)
	}
}

// TestRecordHookEventAt_WholeSecondSortsBeforeFractional (S14a rev 16 MINOR regression guard): a whole-second
// stamp (…03.000000000Z) must sort BEFORE its fractional neighbor (…03.500000000Z). RFC3339Nano trims
// trailing zeros to "…03Z", which sorts AFTER "…03.5Z" because 'Z' (0x5A) > '.' (0x2E) — the fixed-width
// 9-digit layout keeps the trailing zeros so lexicographic order == chronological order.
func TestRecordHookEventAt_WholeSecondSortsBeforeFractional(t *testing.T) {
	a, _ := newTestArchive(t)
	whole := time.Date(2026, 7, 16, 1, 2, 3, 0, time.UTC)        // …03.000000000Z (arrived first)
	frac := time.Date(2026, 7, 16, 1, 2, 3, 500000000, time.UTC) // …03.500000000Z (arrived second)
	if err := a.RecordHookEventAt("Stop", "sess-2", "m-frac", "t", "p", []byte(`{}`), frac); err != nil {
		t.Fatalf("RecordHookEventAt frac: %v", err)
	}
	if err := a.RecordHookEventAt("Stop", "sess-2", "m-whole", "t", "p", []byte(`{}`), whole); err != nil {
		t.Fatalf("RecordHookEventAt whole: %v", err)
	}
	var first string
	if err := a.db.QueryRow(`SELECT message_id FROM hook_events WHERE session_id='sess-2' ORDER BY ts LIMIT 1`).Scan(&first); err != nil {
		t.Fatalf("query: %v", err)
	}
	if first != "m-whole" {
		t.Fatalf("a whole-second stamp must sort BEFORE its fractional neighbor (RFC3339Nano-trim guard), got first=%q", first)
	}
}
