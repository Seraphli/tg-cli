package helpers

import (
	"context"
	"testing"

	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/tgfake"
)

// useTempConfig points config at a fresh temp dir for the test.
func useTempConfig(t *testing.T) {
	t.Helper()
	old := config.ConfigDir
	config.ConfigDir = t.TempDir()
	t.Cleanup(func() { config.ConfigDir = old })
}

// seedRoutes writes routes.json holding exactly the given routes.
func seedRoutes(t *testing.T, routes map[string]config.NameRoute) {
	t.Helper()
	stored, err := config.LoadRoutes()
	if err != nil {
		t.Fatalf("LoadRoutes: %v", err)
	}
	stored.NameRouteMap = routes
	if err := config.SaveRoutes(stored); err != nil {
		t.Fatalf("SaveRoutes: %v", err)
	}
}

func TestFillEmptyRouteTitles(t *testing.T) {
	useTempConfig(t)
	seedRoutes(t, map[string]config.NameRoute{
		"alpha": {ChatID: -100111},
		"beta":  {ChatID: -100222},
		"gamma": {ChatID: -100333, Title: "Kept"},
	})
	s := tgfake.New(t)
	bot := s.Bot(t)
	first := true
	s.On("getChat", func(p map[string]interface{}) tgfake.Reply {
		if first {
			first = false
			err := config.UpdateRoutes(func(r *config.Routes) bool {
				r.NameRouteMap["delta"] = config.NameRoute{ChatID: -100444}
				return true
			})
			if err != nil {
				t.Errorf("UpdateRoutes in script: %v", err)
			}
		}
		switch p["chat_id"] {
		case "-100111":
			return tgfake.Reply{Result: map[string]interface{}{"id": -100111, "type": "supergroup", "title": "Alpha Group"}}
		case "-100222":
			return tgfake.Reply{Err: "Bad Request: chat not found"}
		}
		t.Errorf("unexpected getChat chat_id = %v", p["chat_id"])
		return tgfake.Reply{Err: "Bad Request: chat not found"}
	})
	FillEmptyRouteTitles(context.Background(), bot)
	stored, err := config.LoadRoutes()
	if err != nil {
		t.Fatalf("LoadRoutes: %v", err)
	}
	r := stored.NameRouteMap
	if r["alpha"].Title != "Alpha Group" || r["beta"].Title != "" || r["gamma"].Title != "Kept" {
		t.Fatalf("titles after first run: %+v", r)
	}
	if _, ok := r["delta"]; !ok {
		t.Fatalf("delta added during the fill was lost: %+v", r)
	}
	if s.Count("getChat") != 2 {
		t.Fatalf("getChat count = %d, want 2", s.Count("getChat"))
	}
}
