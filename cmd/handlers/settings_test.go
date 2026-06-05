package handlers

import (
	"testing"

	"github.com/Seraphli/tg-cli/cmd/types"
	tele "gopkg.in/telebot.v3"
)

func TestBuildSettingsTopMenu(t *testing.T) {
	menu := buildSettingsTopMenu()
	if len(menu.InlineKeyboard) < 4 {
		t.Errorf("expected at least 4 rows, got %d", len(menu.InlineKeyboard))
	}
	total := 0
	hasDel := false
	for _, row := range menu.InlineKeyboard {
		total += len(row)
		for _, btn := range row {
			if btn.Unique == "del" {
				hasDel = true
				continue
			}
			if btn.Unique != "settings" {
				t.Errorf("expected Unique='settings' or 'del', got '%s'", btn.Unique)
			}
		}
	}
	if total < 8 {
		t.Errorf("expected at least 8 buttons, got %d", total)
	}
	if !hasDel {
		t.Error("expected a 'del' button in settings top menu")
	}
}

func TestAppendBackButton(t *testing.T) {
	menu := &tele.ReplyMarkup{}
	menu.Inline(menu.Row(menu.Data("Test", "test", "data")))
	before := len(menu.InlineKeyboard)
	appendBackButton(menu)
	after := len(menu.InlineKeyboard)
	if after != before+1 {
		t.Errorf("expected %d rows after append, got %d", before+1, after)
	}
	lastRow := menu.InlineKeyboard[after-1]
	if len(lastRow) != 2 {
		t.Fatalf("expected 2 buttons (Back + Delete), got %d", len(lastRow))
	}
	if lastRow[0].Text != "⬅️ Back" {
		t.Errorf("first button should be Back, got %q", lastRow[0].Text)
	}
	if lastRow[1].Unique != "del" {
		t.Errorf("second button should be del, got Unique=%q", lastRow[1].Unique)
	}
}

func TestBuildPermSubMenu(t *testing.T) {
	menu := buildPermSubMenu("plan")
	found := false
	for _, row := range menu.InlineKeyboard {
		for _, btn := range row {
			if btn.Text == "✅ Plan" {
				found = true
			}
			if btn.Text == "✅ Default" {
				t.Error("Default should not have ✅ when current mode is plan")
			}
		}
	}
	if !found {
		t.Error("Plan button should have ✅ prefix")
	}
	menuAE := buildPermSubMenu("acceptEdits")
	foundAE := false
	for _, row := range menuAE.InlineKeyboard {
		for _, btn := range row {
			if btn.Text == "✅ Accept edits" {
				foundAE = true
			}
		}
	}
	if !foundAE {
		t.Error("Accept edits button should have ✅ prefix when mode is acceptEdits")
	}
}

func TestIsSettingsMenu(t *testing.T) {
	bs := &types.BotState{}
	if IsSettingsMenu(bs, 123) {
		t.Error("should return false for untracked msg")
	}
	bs.SettingsMenuMsgs.Store(123, true)
	if !IsSettingsMenu(bs, 123) {
		t.Error("should return true for tracked msg")
	}
}
