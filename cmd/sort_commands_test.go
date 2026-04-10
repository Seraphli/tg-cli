package cmd

import (
	"testing"

	"github.com/Seraphli/tg-cli/cmd/stores"
	tele "gopkg.in/telebot.v3"
)

func TestBuildSortedCommands(t *testing.T) {
	base := []tele.Command{
		{Text: "alpha", Description: "Alpha"},
		{Text: "beta", Description: "Beta"},
		{Text: "gamma", Description: "Gamma"},
		{Text: "delta", Description: "Delta"},
	}
	pinned := []string{"gamma", "alpha"}
	emptyCounts := map[string]int{}
	emptyCustom := map[string]stores.CustomCmd{}

	t.Run("pinned entries appear first in exact pinned order", func(t *testing.T) {
		result := buildSortedCommands(base, pinned, emptyCounts, emptyCustom)
		// First two must be the pinned ones in order: gamma, alpha
		if result[0].Text != "gamma" {
			t.Errorf("expected result[0]=gamma, got %s", result[0].Text)
		}
		if result[1].Text != "alpha" {
			t.Errorf("expected result[1]=alpha, got %s", result[1].Text)
		}
	})

	t.Run("non-pinned entries sorted by count descending", func(t *testing.T) {
		counts := map[string]int{
			"delta": 10,
			"beta":  5,
		}
		result := buildSortedCommands(base, pinned, counts, emptyCustom)
		// After pinned (gamma, alpha), remaining are beta and delta sorted by count desc
		// delta(10) before beta(5)
		if result[2].Text != "delta" {
			t.Errorf("expected result[2]=delta, got %s", result[2].Text)
		}
		if result[3].Text != "beta" {
			t.Errorf("expected result[3]=beta, got %s", result[3].Text)
		}
	})

	t.Run("ties within non-pinned group use original base registration order", func(t *testing.T) {
		// beta is index 1, delta is index 3 — both count 0, so beta comes first
		result := buildSortedCommands(base, pinned, emptyCounts, emptyCustom)
		// After pinned (gamma=2, alpha=0), remaining order by index: beta(1) before delta(3)
		if result[2].Text != "beta" {
			t.Errorf("expected result[2]=beta, got %s", result[2].Text)
		}
		if result[3].Text != "delta" {
			t.Errorf("expected result[3]=delta, got %s", result[3].Text)
		}
	})

	t.Run("unknown pinned names are silently skipped", func(t *testing.T) {
		unknownPinned := []string{"nonexistent", "gamma"}
		result := buildSortedCommands(base, unknownPinned, emptyCounts, emptyCustom)
		// Only gamma is a valid pinned entry; first must be gamma
		if result[0].Text != "gamma" {
			t.Errorf("expected result[0]=gamma, got %s", result[0].Text)
		}
		// Total base entries count must equal len(base)
		baseCount := 0
		for _, r := range result {
			for _, b := range base {
				if r.Text == b.Text {
					baseCount++
					break
				}
			}
		}
		if baseCount != len(base) {
			t.Errorf("expected %d base entries, got %d", len(base), baseCount)
		}
	})

	t.Run("all counts zero preserves base order for non-pinned", func(t *testing.T) {
		noPinned := []string{}
		result := buildSortedCommands(base, noPinned, emptyCounts, emptyCustom)
		// With no pinned and all counts zero, order must match base order
		for i, b := range base {
			if result[i].Text != b.Text {
				t.Errorf("expected result[%d]=%s, got %s", i, b.Text, result[i].Text)
			}
		}
	})
}
