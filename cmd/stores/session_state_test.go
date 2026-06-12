package stores

import "testing"

// TC1: a full-format query must resolve to the session whose socket+pane match
// exactly, never colliding with the same pane id on a different tmux socket.
func TestFindByTargetFullQueryDistinguishesSockets(t *testing.T) {
	s := NewSessionStateStore(t.TempDir())
	s.sessions["sidA"] = SessionInfo{TmuxTarget: "%1@/tmp/tmux-1000/default", Name: "tg-cli"}
	s.sessions["sidB"] = SessionInfo{TmuxTarget: "%1@/tmp/tmux-1000/abi-cap-test"}
	// Loop to defeat Go map iteration randomness — the result must be deterministic.
	for i := 0; i < 50; i++ {
		sid, ok := s.FindByTarget("%1@/tmp/tmux-1000/default")
		if !ok || sid != "sidA" {
			t.Fatalf("FindByTarget(default) iter %d: got (%q,%v), want (sidA,true)", i, sid, ok)
		}
		info := s.FindInfoByTarget("%1@/tmp/tmux-1000/default")
		if info == nil || info.Name != "tg-cli" {
			t.Fatalf("FindInfoByTarget(default) iter %d: got %+v, want Name=tg-cli", i, info)
		}
		sidB, okB := s.FindByTarget("%1@/tmp/tmux-1000/abi-cap-test")
		if !okB || sidB != "sidB" {
			t.Fatalf("FindByTarget(abi-cap-test) iter %d: got (%q,%v), want (sidB,true)", i, sidB, okB)
		}
	}
}

// TC2: a bare short-format query still matches a stored full-format target by pane id.
func TestFindByTargetShortQueryMatchesFullStored(t *testing.T) {
	s := NewSessionStateStore(t.TempDir())
	s.sessions["sidC"] = SessionInfo{TmuxTarget: "%5@/tmp/tmux-1000/default", Name: "ca"}
	sid, ok := s.FindByTarget("%5")
	if !ok || sid != "sidC" {
		t.Fatalf("FindByTarget(%%5): got (%q,%v), want (sidC,true)", sid, ok)
	}
	info := s.FindInfoByTarget("%5")
	if info == nil || info.Name != "ca" {
		t.Fatalf("FindInfoByTarget(%%5): got %+v, want Name=ca", info)
	}
}

// TC3: a full-format query with no matching socket returns not-found.
func TestFindByTargetFullQueryNoMatch(t *testing.T) {
	s := NewSessionStateStore(t.TempDir())
	s.sessions["sidA"] = SessionInfo{TmuxTarget: "%1@/tmp/tmux-1000/default"}
	if sid, ok := s.FindByTarget("%1@/tmp/tmux-1000/nonexistent"); ok || sid != "" {
		t.Fatalf("FindByTarget(nonexistent socket): got (%q,%v), want (\"\",false)", sid, ok)
	}
	if info := s.FindInfoByTarget("%1@/tmp/tmux-1000/nonexistent"); info != nil {
		t.Fatalf("FindInfoByTarget(nonexistent socket): got %+v, want nil", info)
	}
}
