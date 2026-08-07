package helpers

import (
	"strings"
	"testing"

	"github.com/Seraphli/tg-cli/cmd/stores"
)

// Fix 18: after toggling an option in a multiSelect AskUserQuestion, the rebuilt keyboard must still
// contain the ❌ Cancel button (it was being dropped, so Cancel disappeared after any toggle).
func TestRebuildAskMarkupKeepsCancel(t *testing.T) {
	q := stores.QuestionMeta{
		QuestionText:    "Pick colors",
		NumOptions:      3,
		OptionLabels:    []string{"Red", "Blue", "Green"},
		MultiSelect:     true,
		SelectedOptions: map[int]bool{0: true}, // Red toggled on
	}
	markup := RebuildAskMarkup([]stores.QuestionMeta{q})
	hasCancel := false
	hasCheck := false
	for _, row := range markup.InlineKeyboard {
		for _, b := range row {
			if b.Data == "AskUserQuestion|cancel" || b.Text == "❌ Cancel" {
				hasCancel = true
			}
			if strings.HasPrefix(b.Text, "✅ ") {
				hasCheck = true
			}
		}
	}
	if !hasCancel {
		t.Errorf("rebuilt multiSelect keyboard must keep the ❌ Cancel button (Fix 18)")
	}
	if !hasCheck {
		t.Errorf("the toggled option should carry a ✅ prefix")
	}
}
