package handlers

import (
	"testing"

	"github.com/Seraphli/tg-cli/cmd/types"
	tele "gopkg.in/telebot.v3"
)

func TestBuildSettingsTopMenu(t *testing.T) {
	menu := buildSettingsTopMenu()
	if len(menu.InlineKeyboard) != 4 {
		t.Errorf("expected 4 rows, got %d", len(menu.InlineKeyboard))
	}
	total := 0
	for _, row := range menu.InlineKeyboard {
		total += len(row)
	}
	if total != 8 {
		t.Errorf("expected 8 buttons, got %d", total)
	}
	for _, row := range menu.InlineKeyboard {
		for _, btn := range row {
			if btn.Unique != "settings" {
				t.Errorf("expected Unique='settings', got '%s'", btn.Unique)
			}
		}
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
	if len(lastRow) != 1 || lastRow[0].Text != "⬅️ Back" {
		t.Errorf("last row should be Back button, got %v", lastRow)
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
