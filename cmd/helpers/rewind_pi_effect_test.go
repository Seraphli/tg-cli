package helpers

import "testing"

// pi: the dialog has just closed and an earlier "Navigated to selected point" line is on screen from a navigation before, the editor is empty (F1 of every fixture). piEffect must not confirm that pane (RewindDrive clears the editor before the action), nor the "Summarizing branch..." pane, nor a summarize that was aborted and left the editor empty (testdata/pi_effect_aborted.txt, frames F1 to F4 of a live run: the pane before, the spinner, "Branch summarization cancelled" with the tree open, the pane after the Escape); it confirms only the pane after the navigation with the picked message back in the editor: testdata/pi_effect_summarize.txt (F3, after the spinner of F2) and testdata/pi_effect_nosummary.txt (F2).
func TestRewindPiEffect(t *testing.T) {
	for _, c := range []struct {
		fixture string
		want    []bool
	}{{"pi_effect_summarize.txt", []bool{false, false, true}}, {"pi_effect_nosummary.txt", []bool{false, true}}, {"pi_effect_aborted.txt", []bool{false, false, false, false}}} {
		frames := fixtureFrames(t, c.fixture)
		if len(frames) != len(c.want) {
			t.Errorf("%s: %d frames, want %d", c.fixture, len(frames), len(c.want))
			continue
		}
		for i, v := range frames {
			got := piEffect(v)
			if got && !c.want[i] {
				t.Errorf("%s F%d: piEffect confirmed before the summary started", c.fixture, i+1)
			}
			if !got && c.want[i] {
				t.Errorf("%s F%d: piEffect did not confirm after the navigation", c.fixture, i+1)
			}
		}
	}
}
