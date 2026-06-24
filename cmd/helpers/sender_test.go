package helpers

import (
	"testing"

	tele "gopkg.in/telebot.v3"
)

func TestFreezeEditArgs(t *testing.T) {
	markup := &tele.ReplyMarkup{}
	t.Run("empty text returns markup-only", func(t *testing.T) {
		what, opts := freezeEditArgs("", markup)
		if _, ok := what.(*tele.ReplyMarkup); !ok {
			t.Fatalf("expected *tele.ReplyMarkup, got %T", what)
		}
		if what != markup {
			t.Fatal("expected the same markup pointer")
		}
		if opts != nil {
			t.Fatalf("expected nil opts for markup-only, got %v", opts)
		}
	})
	t.Run("non-empty text returns text with markup and HTML mode", func(t *testing.T) {
		what, opts := freezeEditArgs("hello", markup)
		s, ok := what.(string)
		if !ok {
			t.Fatalf("expected string, got %T", what)
		}
		if s != "hello" {
			t.Fatalf("expected hello, got %q", s)
		}
		if len(opts) != 2 {
			t.Fatalf("expected 2 opts, got %d", len(opts))
		}
		if opts[0] != markup {
			t.Fatal("expected markup as first opt")
		}
		if opts[1] != tele.ModeHTML {
			t.Fatalf("expected ModeHTML, got %v", opts[1])
		}
	})
}
