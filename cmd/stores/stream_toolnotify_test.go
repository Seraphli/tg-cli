package stores

import "testing"

// TestGetAndClearLastToolNotify_SecondCallFindsNothing (item 7 bullet 5, fix C option 3): a second same-
// prompt consumption of the mark finds nothing, since the first call already cleared it — proving a second
// same-prompt final MessageDisplay no longer logs "late MD after tool notify" (register.go:1002's `ok` is
// false on the second occurrence). Without the clear-on-read (the old read-only GetLastToolNotify), this
// test would go red (the second call would still return ok=true).
func TestGetAndClearLastToolNotify_SecondCallFindsNothing(t *testing.T) {
	s := NewStreamStore()
	s.SetLastToolNotify("sess", "p1")
	mark, ok := s.GetAndClearLastToolNotify("sess")
	if !ok || mark.PromptID != "p1" {
		t.Fatalf("first call: expected (PromptID=%q, ok=true), got (%+v, %v)", "p1", mark, ok)
	}
	mark2, ok2 := s.GetAndClearLastToolNotify("sess")
	if ok2 {
		t.Fatalf("second call: expected ok=false (mark already consumed by the first call), got (%+v, %v)", mark2, ok2)
	}
}
