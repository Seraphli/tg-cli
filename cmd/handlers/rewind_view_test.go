package handlers

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Seraphli/tg-cli/cmd/helpers"
	tele "gopkg.in/telebot.v3"
)

// rewindTestMenu is a cc menu of 25 messages, newest first, at the given page; entry i is "message <i+1>", so the number in front of a label must equal the number inside it.
func rewindTestMenu(page int) *rewindMenu {
	menu := &rewindMenu{Backend: "cc", Page: page, Picked: -1}
	for i := range 25 {
		menu.Entries = append(menu.Entries, helpers.RewindEntry{Label: fmt.Sprintf("message %d", i+1), Annotation: "No code changes"})
	}
	return menu
}

// keyboardTexts returns the text of each keyboard row and fails the test when a row does not hold exactly one button.
func keyboardTexts(t *testing.T, kb *tele.ReplyMarkup) []string {
	t.Helper()
	var texts []string
	for _, row := range kb.InlineKeyboard {
		if len(row) != 1 {
			t.Fatalf("a keyboard row holds %d buttons, want 1: %+v", len(row), kb.InlineKeyboard)
		}
		texts = append(texts, row[0].Text)
	}
	return texts
}

// TestRewindListPages renders the three pages of a 25-message list as the bot sends them: the numbers start at the newest message and continue on the older pages (11 to 20 on the second), every row holds one button, "⬆️ Older" is on top while older messages exist, "⬇️ Newer" and "❌ Cancel" each take a row of their own, and every entry button, taken by position, carries the unique rw_pick and the data N-1, so the button N picks the message labelled N.
func TestRewindListPages(t *testing.T) {
	for _, p := range []struct{ page, first, last int }{{0, 1, 10}, {10, 11, 20}, {20, 21, 25}} {
		text, kb := rewindListView(rewindTestMenu(p.page), "work:0.1")
		var entries, body []string
		for n := p.last; n >= p.first; n-- {
			entries = append(entries, fmt.Sprintf("%d. message %d", n, n))
		}
		want := slices.Concat(entries, []string{"❌ Cancel"})
		if p.last < 25 {
			want = slices.Insert(want, 0, "⬆️ Older")
		}
		if p.page > 0 {
			want = slices.Insert(want, len(want)-1, "⬇️ Newer")
		}
		if got := keyboardTexts(t, kb); !slices.Equal(got, want) {
			t.Errorf("page %d: keyboard rows %q, want %q", p.page, got, want)
		}
		lines := strings.Split(text, "\n")
		for _, l := range lines {
			if l != "" && l[0] >= '0' && l[0] <= '9' {
				body = append(body, l)
			}
		}
		if lines[1] != "Newest at the bottom" || !slices.Equal(body, entries) {
			t.Errorf("page %d: body %q, want line 2 \"Newest at the bottom\" and the numbered lines %q", p.page, text, entries)
		}
		top := slices.Index(want, entries[0])
		for k := 0; k < len(entries) && top+k < len(kb.InlineKeyboard); k++ {
			if b := kb.InlineKeyboard[top+k][0]; b.Unique != "rw_pick" || b.Data != strconv.Itoa(p.last-k-1) {
				t.Errorf("page %d: entry button %q has the unique %q and the data %q, want rw_pick and %d", p.page, b.Text, b.Unique, b.Data, p.last-k-1)
			}
		}
	}
}

// TestRewindActionPage renders the action page of message 13 (entry 12, on the page that starts at 10): it carries the number of the list, every row holds one button, and "◀️ Back" and "❌ Cancel" each take a row of their own.
func TestRewindActionPage(t *testing.T) {
	menu := rewindTestMenu(10)
	menu.Picked = 12
	text, kb := rewindActionView(menu)
	got := keyboardTexts(t, kb)
	if !strings.HasPrefix(text, "13. message 13") || !slices.Equal(got[len(got)-2:], []string{"◀️ Back", "❌ Cancel"}) {
		t.Errorf("action page text %q, rows %q: want the text to start with \"13. message 13\" and the last rows ◀️ Back and ❌ Cancel", text, got)
	}
}
