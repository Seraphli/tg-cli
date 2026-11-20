package config

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Seraphli/tg-cli/internal/logger"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// Release drops both memory copies and goes back to reading the files on every call.
func Release() {
	storeMu.Lock()
	defer storeMu.Unlock()
	held = false
	routesStore.mem, credsStore.mem = nil, nil
}

// routeNames is the sorted route names of r.
func routeNames(r Routes) []string { return slices.Sorted(maps.Keys(r.NameRouteMap)) }

// loadedRoutes is LoadRoutes: the memory copy while held, the file otherwise.
func loadedRoutes(t *testing.T) Routes {
	t.Helper()
	r, err := LoadRoutes()
	must(t, err)
	return r
}

// routesOnDisk decodes routes.json itself, never the memory copy.
func routesOnDisk(t *testing.T) Routes {
	t.Helper()
	data, err := os.ReadFile(routesStore.path())
	must(t, err)
	var r Routes
	must(t, json.Unmarshal(data, &r))
	return r
}

// Two binds in the same moment (two /bot_bind from two chats, or a bind while a SessionStart migrates a route): both survive.
func TestUpdateRoutesConcurrentBinds(t *testing.T) {
	ConfigDir = t.TempDir()
	t.Cleanup(func() { Release(); ConfigDir = "" })
	must(t, Hold())
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := UpdateRoutes(func(r *Routes) bool {
				time.Sleep(time.Millisecond)
				r.NameRouteMap[fmt.Sprintf("s%d", i)] = NameRoute{ChatID: int64(i)}
				return true
			})
			if err != nil {
				t.Errorf("UpdateRoutes s%d: %v", i, err)
			}
		}()
	}
	wg.Wait()
	if n := len(loadedRoutes(t).NameRouteMap); n != 32 {
		t.Errorf("memory holds %d routes, want 32", n)
	}
	if n := len(routesOnDisk(t).NameRouteMap); n != 32 {
		t.Errorf("routes.json holds %d routes, want 32", n)
	}
}

// The bot runs for days and never reads the file again; a bind is visible to the very next lookup.
func TestHeldRoutesServedFromMemory(t *testing.T) {
	ConfigDir = t.TempDir()
	t.Cleanup(func() { Release(); ConfigDir = "" })
	must(t, SaveRoutes(Routes{NameRouteMap: map[string]NameRoute{"a": {ChatID: 1}}}))
	must(t, Hold())
	if got := routeNames(loadedRoutes(t)); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("after Hold: routes %v, want [a]", got)
	}
	must(t, os.Remove(routesStore.path()))
	if got := routeNames(loadedRoutes(t)); !slices.Equal(got, []string{"a"}) {
		t.Errorf("after the file was deleted: routes %v, want [a] from memory", got)
	}
	must(t, UpdateRoutes(func(r *Routes) bool {
		r.NameRouteMap["b"] = NameRoute{ChatID: 2}
		return true
	}))
	if got := routeNames(loadedRoutes(t)); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("right after the bind: routes %v, want [a b]", got)
	}
	if got := routeNames(routesOnDisk(t)); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("routes.json holds %v, want [a b]", got)
	}
}

// routes.json cannot be written (disk full, permissions) while someone binds: the caller gets the error and the bot keeps the old route.
func TestUpdateRoutesWriteFailureKeepsOldBinding(t *testing.T) {
	ConfigDir = t.TempDir()
	t.Cleanup(func() { Release(); ConfigDir = "" })
	must(t, SaveRoutes(Routes{NameRouteMap: map[string]NameRoute{"a": {ChatID: 1}}}))
	must(t, Hold())
	must(t, os.Remove(routesStore.path()))
	must(t, os.Mkdir(routesStore.path(), 0700))
	err := UpdateRoutes(func(r *Routes) bool {
		r.NameRouteMap["x"] = NameRoute{ChatID: 9}
		return true
	})
	if err == nil {
		t.Fatal("UpdateRoutes returned nil although routes.json cannot be written")
	}
	if got := routeNames(loadedRoutes(t)); !slices.Equal(got, []string{"a"}) {
		t.Errorf("after the failed bind: routes %v, want [a]", got)
	}
}

// A mistyped file at `tg-cli reload`, routes.json (read first) or credentials.json (read second): the bot keeps the old routes and the old
// credentials, and the next reload applies the fix.
func TestReloadKeepsMemoryWhenAFileDoesNotParse(t *testing.T) {
	ConfigDir = t.TempDir()
	t.Cleanup(func() { Release(); ConfigDir = "" })
	must(t, SaveRoutes(Routes{NameRouteMap: map[string]NameRoute{"a": {ChatID: 1}}}))
	must(t, SaveCredentials(Credentials{Port: 12501}))
	must(t, Hold())
	must(t, os.WriteFile(routesStore.path(), []byte(`{"nameRouteMap": {"a": {"chatId"`), 0600))
	must(t, writeJSON(credsStore.path(), Credentials{Port: 12600}))
	if _, err := Reload(); err == nil || !strings.Contains(err.Error(), "routes.json") {
		t.Fatalf("Reload error = %v, want one naming routes.json", err)
	}
	if got := routeNames(loadedRoutes(t)); !slices.Equal(got, []string{"a"}) {
		t.Errorf("after the failed reload: routes %v, want [a]", got)
	}
	if c, err := LoadCredentials(); err != nil || c.Port != 12501 {
		t.Errorf("after the failed reload: port %d (err %v), want 12501", c.Port, err)
	}
	must(t, writeJSON(routesStore.path(), Routes{NameRouteMap: map[string]NameRoute{"c": {ChatID: 3}}}))
	must(t, os.WriteFile(credsStore.path(), []byte(`{"port": 126`), 0600))
	if _, err := Reload(); err == nil || !strings.Contains(err.Error(), "credentials.json") {
		t.Fatalf("Reload error = %v, want one naming credentials.json", err)
	}
	if got := routeNames(loadedRoutes(t)); !slices.Equal(got, []string{"a"}) {
		t.Errorf("after the failed reload of a cut-off credentials.json: routes %v, want [a]", got)
	}
	if c, err := LoadCredentials(); err != nil || c.Port != 12501 {
		t.Errorf("after the failed reload of a cut-off credentials.json: port %d (err %v), want 12501", c.Port, err)
	}
	must(t, writeJSON(credsStore.path(), Credentials{Port: 12600}))
	counts, err := Reload()
	if err != nil || counts != "routes=1 forumTopics=0 mailbox=false pairingIds=0" {
		t.Fatalf("second Reload = %q, %v", counts, err)
	}
	if got := routeNames(loadedRoutes(t)); !slices.Equal(got, []string{"c"}) {
		t.Errorf("after the second reload: routes %v, want [c]", got)
	}
	if c, err := LoadCredentials(); err != nil || c.Port != 12600 {
		t.Errorf("after the second reload: port %d (err %v), want 12600", c.Port, err)
	}
}

// The production upgrade starts the new bot on a credentials.json with routes while routes.json cannot be written: the old file stays
// as it was, and the next start migrates.
func TestMigrateRoutesFailureKeepsCredentials(t *testing.T) {
	ConfigDir = t.TempDir()
	t.Cleanup(func() { Release(); ConfigDir = "" })
	seed := []byte(`{"botToken":"test-token","pairingAllow":{"ids":["1"],"defaultChatId":""},"port":12501,"nameRouteMap":{"a":{"chatId":1},"b":{"chatId":2}}}`)
	must(t, os.WriteFile(credsStore.path(), seed, 0600))
	must(t, os.Mkdir(routesStore.path()+".tmp", 0700))
	if err := MigrateRoutes(); err == nil || !strings.Contains(err.Error(), "step b") {
		t.Fatalf("MigrateRoutes error = %v, want one naming step b", err)
	}
	if got, err := os.ReadFile(credsStore.path()); err != nil || string(got) != string(seed) {
		t.Errorf("credentials.json changed after the failed migration (read error %v)", err)
	}
	if _, err := os.Stat(routesStore.path()); !os.IsNotExist(err) {
		t.Errorf("routes.json exists after the failed migration (stat error %v)", err)
	}
	must(t, os.Remove(routesStore.path()+".tmp"))
	must(t, MigrateRoutes())
	if got := routeNames(loadedRoutes(t)); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("after the retry: routes.json holds %v, want [a b]", got)
	}
	data, err := os.ReadFile(credsStore.path())
	must(t, err)
	var fields map[string]json.RawMessage
	must(t, json.Unmarshal(data, &fields))
	for _, moved := range []string{"nameRouteMap", "forumTopics", "mailboxChatId"} {
		if _, ok := fields[moved]; ok {
			t.Errorf("credentials.json still holds %s after the migration", moved)
		}
	}
	if c, err := LoadCredentials(); err != nil || c.BotToken != "test-token" || c.Port != 12501 {
		t.Errorf("credentials after the migration: port %d (err %v), want the token and port 12501 kept", c.Port, err)
	}
}

// A rollback to the old binary, a bind it wrote into credentials.json, then the new bot starts again with routes.json still there: routes.json
// wins and the log says what happens to the old fields; an ordinary restart logs nothing.
func TestMigrateRoutesWarnsWhenCredentialsHoldBindingsAgain(t *testing.T) {
	ConfigDir = t.TempDir()
	t.Cleanup(func() { Release(); ConfigDir = "" })
	logPath := filepath.Join(ConfigDir, "bot.log")
	logger.Init(logPath, false)
	t.Cleanup(func() { logger.Init(os.DevNull, false) })
	must(t, os.WriteFile(credsStore.path(), []byte(`{"botToken":"test-token","port":12501,"nameRouteMap":{"a":{"chatId":1}}}`), 0600))
	must(t, MigrateRoutes())
	must(t, MigrateRoutes())
	if got, _ := os.ReadFile(logPath); strings.Contains(string(got), "still holds") {
		t.Errorf("an ordinary restart logged a warning: %s", got)
	}
	left := []byte(`{"botToken":"test-token","port":12501,"nameRouteMap":{"a":{"chatId":1},"b":{"chatId":2}}}`)
	must(t, os.WriteFile(credsStore.path(), left, 0600))
	must(t, MigrateRoutes())
	if got, _ := os.ReadFile(logPath); !strings.Contains(string(got), "[WARN] credentials.json still holds") {
		t.Errorf("no warning after the old binary bound into credentials.json: %q", got)
	}
	if got := routeNames(routesOnDisk(t)); !slices.Equal(got, []string{"a"}) {
		t.Errorf("routes.json holds %v, want [a]: it wins", got)
	}
	if got, err := os.ReadFile(credsStore.path()); err != nil || string(got) != string(left) {
		t.Errorf("credentials.json changed by the start (read error %v)", err)
	}
}
