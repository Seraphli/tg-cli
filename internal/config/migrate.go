package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/Seraphli/tg-cli/internal/logger"
)

// credentialsBackupFile is the byte-for-byte copy of credentials.json that MigrateRoutes leaves next to it.
const credentialsBackupFile = "credentials.json.pre-routes"

// MigrateRoutes moves nameRouteMap, forumTopics and mailboxChatId out of credentials.json into routes.json. It changes
// nothing when routes.json exists (warnBindingsLeft only logs) or credentials.json holds none of the three. Otherwise:
// (a) copy credentials.json to the backup file and compare, (b) write routes.json and read it back, (c) only then
// rewrite credentials.json without the three fields. A failed step returns an error and credentials.json is as it was.
func MigrateRoutes() error {
	storeMu.Lock()
	defer storeMu.Unlock()
	if _, err := os.Stat(routesStore.path()); err == nil {
		warnBindingsLeft()
		return nil
	}
	old, err := os.ReadFile(credsStore.path())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read credentials.json: %w", err)
	}
	var routes Routes
	var creds Credentials
	if err := json.Unmarshal(old, &routes); err != nil {
		return fmt.Errorf("parse credentials.json: %w", err)
	}
	if err := json.Unmarshal(old, &creds); err != nil {
		return fmt.Errorf("parse credentials.json: %w", err)
	}
	if len(routes.NameRouteMap) == 0 && len(routes.ForumTopics) == 0 && routes.MailboxChatID == 0 {
		return nil
	}
	routesStore.fill(&routes)
	credsStore.fill(&creds)
	backup := filepath.Join(GetConfigDir(), credentialsBackupFile)
	if err := os.WriteFile(backup, old, 0600); err != nil {
		return fmt.Errorf("step a, backup: %w", err)
	}
	if got, err := os.ReadFile(backup); err != nil || !bytes.Equal(got, old) {
		return fmt.Errorf("step a, the backup does not match credentials.json (read error: %v)", err)
	}
	if err := writeJSON(routesStore.path(), routes); err != nil {
		return fmt.Errorf("step b, write routes.json: %w", err)
	}
	var back Routes
	data, err := os.ReadFile(routesStore.path())
	if err == nil {
		err = json.Unmarshal(data, &back)
	}
	routesStore.fill(&back)
	if err != nil || !reflect.DeepEqual(routes, back) {
		os.Remove(routesStore.path())
		return fmt.Errorf("step b, routes.json read back differs from what was written (read error: %v)", err)
	}
	if err := writeJSON(credsStore.path(), creds); err != nil {
		return fmt.Errorf("step c, rewrite credentials.json: %w", err)
	}
	logger.Info(fmt.Sprintf("Bindings migrated to routes.json: %s backup=%s", routesStore.count(routes), credentialsBackupFile))
	return nil
}

// warnBindingsLeft logs one warning when credentials.json still holds bindings although routes.json exists: the old binary
// ran in between (a rollback) and the new one starts again. It only reads and never fails a start.
func warnBindingsLeft() {
	data, err := os.ReadFile(credsStore.path())
	var left Routes
	if err != nil || json.Unmarshal(data, &left) != nil {
		return
	}
	if len(left.NameRouteMap) > 0 || len(left.ForumTopics) > 0 || left.MailboxChatID != 0 {
		logger.Warn("credentials.json still holds nameRouteMap, forumTopics or mailboxChatId next to routes.json: routes.json wins, those fields are ignored and are dropped at the next credentials write. To migrate them instead: stop the bot, delete routes.json, start the bot again.")
	}
}
