package helpers

import (
	"os"
	"time"
)

type dirEntry struct {
	name    string
	isDir   bool
	modTime time.Time
}

func getHomeDir() (string, error) {
	return os.UserHomeDir()
}

func readDir(dir string) ([]dirEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var result []dirEntry
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		result = append(result, dirEntry{
			name:    e.Name(),
			isDir:   e.IsDir(),
			modTime: info.ModTime(),
		})
	}
	return result, nil
}
