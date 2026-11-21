package logger

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// appendWriter is the bot.log writer. It always opens the file with O_APPEND, so a line another process
// appended (every hook process appends to bot.log) is never overwritten by the bot's next write, and it reads
// the size from the open file, so those lines count toward the rotation size. At maxBytes the file is renamed to
// <name>-<UTC timestamp><ext> and the keep newest backups stay.
type appendWriter struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	keep     int
	f        *os.File
}

func newAppendWriter(path string, maxBytes int64, keep int) *appendWriter {
	return &appendWriter{path: path, maxBytes: maxBytes, keep: keep}
}

func (w *appendWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	if fi, err := w.f.Stat(); err == nil && fi.Size() > 0 && fi.Size()+int64(len(p)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	return w.f.Write(p)
}

func (w *appendWriter) open() error {
	if err := os.MkdirAll(filepath.Dir(w.path), 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	w.f = f
	return err
}

func (w *appendWriter) rotate() error {
	w.f.Close()
	w.f = nil // a failed rename or reopen below must not leave a closed file for the next Write
	ext := filepath.Ext(w.path)
	prefix := strings.TrimSuffix(w.path, ext) + "-"
	if err := os.Rename(w.path, prefix+time.Now().UTC().Format("2006-01-02T15-04-05.000")+ext); err != nil {
		return fmt.Errorf("rotate %s: %w", w.path, err)
	}
	backups, _ := filepath.Glob(prefix + "*" + ext)
	sort.Strings(backups)
	for len(backups) > w.keep {
		os.Remove(backups[0])
		backups = backups[1:]
	}
	return w.open()
}
