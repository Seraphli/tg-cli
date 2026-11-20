package config

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/Seraphli/tg-cli/internal/logger"
)

var (
	storeMu sync.Mutex // one lock for both stores: every read, change, reload and migration holds it
	held    bool       // true in the bot process (Hold): each file is read once and kept in memory
)

// store is one JSON file of the config dir. While held, mem is the only copy the process reads: every change
// replaces mem first and then writes the file. Otherwise every call reads the file. Every method needs storeMu.
type store[T any] struct {
	file  string         // file name in the config dir
	what  string         // first word of the log lines
	clone func(T) T      // deep copy
	fill  func(*T)       // defaults for absent fields, applied after every read and every save
	count func(T) string // counts for the log lines, never values
	mem   *T
}

func (s *store[T]) path() string { return filepath.Join(GetConfigDir(), s.file) }

// read parses the file; an absent file gives the defaults.
func (s *store[T]) read() (T, error) {
	var v T
	if err := ensureConfigDir(); err != nil {
		return v, err
	}
	data, err := os.ReadFile(s.path())
	if err == nil {
		err = json.Unmarshal(data, &v)
	} else if os.IsNotExist(err) {
		err = nil
	}
	if err != nil {
		return v, fmt.Errorf("%s: %w", s.file, err)
	}
	s.fill(&v)
	return v, nil
}

// load returns a private copy of the current value; while held, the first call reads the file and later calls read memory.
func (s *store[T]) load() (T, error) {
	if !held {
		return s.read()
	}
	if s.mem == nil {
		v, err := s.read()
		if err != nil {
			return v, err
		}
		s.mem = &v
		logger.Info(fmt.Sprintf("%s loaded from %s: %s", s.what, s.file, s.count(v)))
	}
	return s.clone(*s.mem), nil
}

// update hands fn a private copy of the current value and, when fn returns true, saves the copy.
func (s *store[T]) update(fn func(*T) bool) error {
	v, err := s.load()
	if err != nil {
		return err
	}
	if !fn(&v) {
		return nil
	}
	return s.save(v)
}

// save replaces the memory copy first and then writes the file; a failed write puts the previous copy back.
func (s *store[T]) save(v T) error {
	prev := s.mem
	if held {
		next := s.clone(v)
		s.fill(&next)
		s.mem = &next
	}
	if err := writeJSON(s.path(), v); err != nil {
		s.mem = prev
		return err
	}
	if held {
		logger.Info(fmt.Sprintf("%s changed: %s", s.what, s.count(v)))
	}
	return nil
}

// writeJSON writes v as indented JSON through a temporary file and a rename, so a crash never leaves a half-written file.
func writeJSON(path string, v any) error {
	if err := ensureConfigDir(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

var credsStore = &store[Credentials]{
	file: "credentials.json",
	what: "Credentials",
	clone: func(c Credentials) Credentials {
		c.PairingAllow.IDs = slices.Clone(c.PairingAllow.IDs)
		return c
	},
	fill: func(c *Credentials) {
		if c.PairingAllow.IDs == nil {
			c.PairingAllow.IDs = []string{}
		}
		if c.Port == 0 {
			c.Port = 12500
		}
	},
	count: func(c Credentials) string { return fmt.Sprintf("pairingIds=%d", len(c.PairingAllow.IDs)) },
}

var routesStore = &store[Routes]{
	file: "routes.json",
	what: "Bindings",
	clone: func(r Routes) Routes {
		r.NameRouteMap = maps.Clone(r.NameRouteMap)
		r.ForumTopics = maps.Clone(r.ForumTopics)
		return r
	},
	fill: func(r *Routes) {
		if r.NameRouteMap == nil {
			r.NameRouteMap = map[string]NameRoute{}
		}
		if r.ForumTopics == nil {
			r.ForumTopics = map[string]int{}
		}
	},
	count: func(r Routes) string {
		return fmt.Sprintf("routes=%d forumTopics=%d mailbox=%t", len(r.NameRouteMap), len(r.ForumTopics), r.MailboxChatID != 0)
	},
}

// Hold makes this process (the bot) keep both files in memory: it reads each file once now; from then on every
// read comes from memory and every change goes to memory first and then to its file. Any other process keeps
// reading the files on every call.
func Hold() error {
	storeMu.Lock()
	defer storeMu.Unlock()
	held = true
	if _, err := routesStore.load(); err != nil {
		return err
	}
	_, err := credsStore.load()
	return err
}

// Reload reads both files and, only when both read, replaces both memory copies; it returns the counts. A file that
// does not parse leaves the memory as it was.
func Reload() (string, error) {
	storeMu.Lock()
	defer storeMu.Unlock()
	r, err := routesStore.read()
	if err != nil {
		return "", err
	}
	c, err := credsStore.read()
	if err != nil {
		return "", err
	}
	routesStore.mem, credsStore.mem = &r, &c
	counts := routesStore.count(r) + " " + credsStore.count(c)
	logger.Info("Bindings reloaded: " + counts)
	return counts, nil
}

// LoadCredentials returns a private copy of credentials.json.
func LoadCredentials() (Credentials, error) {
	storeMu.Lock()
	defer storeMu.Unlock()
	return credsStore.load()
}

// SaveCredentials replaces credentials.json with c: memory first, then the file.
func SaveCredentials(c Credentials) error {
	storeMu.Lock()
	defer storeMu.Unlock()
	return credsStore.save(c)
}

// UpdateCredentials hands fn a private copy of the current credentials and, when fn returns true, saves the copy.
// Two changes at the same moment run one after the other, so both survive. fn must not call a config function.
func UpdateCredentials(fn func(*Credentials) bool) error {
	storeMu.Lock()
	defer storeMu.Unlock()
	return credsStore.update(fn)
}

// LoadRoutes returns a private copy of routes.json.
func LoadRoutes() (Routes, error) {
	storeMu.Lock()
	defer storeMu.Unlock()
	return routesStore.load()
}

// SaveRoutes replaces routes.json with r: memory first, then the file.
func SaveRoutes(r Routes) error {
	storeMu.Lock()
	defer storeMu.Unlock()
	return routesStore.save(r)
}

// UpdateRoutes hands fn a private copy of the current routes and, when fn returns true, saves the copy.
// Two changes at the same moment run one after the other, so both survive. fn must not call a config function.
func UpdateRoutes(fn func(*Routes) bool) error {
	storeMu.Lock()
	defer storeMu.Unlock()
	return routesStore.update(fn)
}
