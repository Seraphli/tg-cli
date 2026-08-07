package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Seraphli/tg-cli/internal/config"
)

// initMessageArchive returns nil AND touches no disk when the archive is disabled, and a live handle
// (with the db file created) when enabled.
func TestInitMessageArchive(t *testing.T) {
	t.Run("disabled: nil and no file", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "messages.db")
		off := false
		a := initMessageArchive(config.AppConfig{MessageArchiveEnabled: &off}, dbPath)
		if a != nil {
			a.Close()
			t.Fatalf("disabled archive should be nil")
		}
		if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
			t.Fatalf("disabled archive must not create the db file (stat err=%v)", err)
		}
	})
	t.Run("enabled: non-nil and file exists", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "messages.db")
		a := initMessageArchive(config.AppConfig{}, dbPath) // nil pointer defaults to enabled
		if a == nil {
			t.Fatalf("enabled archive should be non-nil")
		}
		defer a.Close()
		if _, err := os.Stat(dbPath); err != nil {
			t.Fatalf("enabled archive must create the db file: %v", err)
		}
	})
}
