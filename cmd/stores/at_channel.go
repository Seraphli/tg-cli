package stores

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/Seraphli/tg-cli/internal/logger"
)

type AtChannelStore struct {
	mu         sync.RWMutex
	initiators map[string]map[string]bool
	targets    map[string]map[string]bool
	buffers    map[string][]string
	configDir  string
}

func NewAtChannelStore(configDir string) *AtChannelStore {
	return &AtChannelStore{
		initiators: make(map[string]map[string]bool),
		targets:    make(map[string]map[string]bool),
		buffers:    make(map[string][]string),
		configDir:  configDir,
	}
}

func (s *AtChannelStore) Open(initiator, target string) bool {
	s.mu.Lock()
	if s.initiators[initiator] != nil && s.initiators[initiator][target] {
		s.mu.Unlock()
		return false
	}
	if s.initiators[initiator] == nil {
		s.initiators[initiator] = make(map[string]bool)
	}
	if s.targets[target] == nil {
		s.targets[target] = make(map[string]bool)
	}
	s.initiators[initiator][target] = true
	s.targets[target][initiator] = true
	s.mu.Unlock()
	go s.save()
	return true
}

func (s *AtChannelStore) Close(initiator, target string) bool {
	s.mu.Lock()
	if s.initiators[initiator] == nil || !s.initiators[initiator][target] {
		if s.initiators[target] != nil && s.initiators[target][initiator] {
			initiator, target = target, initiator
		} else {
			s.mu.Unlock()
			return false
		}
	}
	delete(s.initiators[initiator], target)
	if len(s.initiators[initiator]) == 0 {
		delete(s.initiators, initiator)
		delete(s.buffers, initiator)
	}
	delete(s.targets[target], initiator)
	if len(s.targets[target]) == 0 {
		delete(s.targets, target)
	}
	s.mu.Unlock()
	go s.save()
	return true
}

func (s *AtChannelStore) CloseAll(name string) []string {
	s.mu.Lock()
	var peers []string
	if tgts, ok := s.initiators[name]; ok {
		for t := range tgts {
			peers = append(peers, t)
			delete(s.targets[t], name)
			if len(s.targets[t]) == 0 {
				delete(s.targets, t)
			}
		}
		delete(s.initiators, name)
		delete(s.buffers, name)
	}
	if inits, ok := s.targets[name]; ok {
		for i := range inits {
			peers = append(peers, i)
			delete(s.initiators[i], name)
			if len(s.initiators[i]) == 0 {
				delete(s.initiators, i)
			}
		}
		delete(s.targets, name)
	}
	s.mu.Unlock()
	go s.save()
	return peers
}

func (s *AtChannelStore) GetTargets(initiator string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	tgts := s.initiators[initiator]
	result := make([]string, 0, len(tgts))
	for t := range tgts {
		result = append(result, t)
	}
	return result
}

func (s *AtChannelStore) GetInitiators(target string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	inits := s.targets[target]
	result := make([]string, 0, len(inits))
	for i := range inits {
		result = append(result, i)
	}
	return result
}

func (s *AtChannelStore) Has(initiator, target string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.initiators[initiator] != nil && s.initiators[initiator][target]
}

func (s *AtChannelStore) save() {
	s.mu.RLock()
	type channelData struct {
		Initiators map[string][]string `json:"initiators"`
	}
	data := channelData{Initiators: make(map[string][]string)}
	for init, tgts := range s.initiators {
		for t := range tgts {
			data.Initiators[init] = append(data.Initiators[init], t)
		}
	}
	s.mu.RUnlock()
	b, _ := json.MarshalIndent(data, "", "  ")
	path := filepath.Join(s.configDir, "at-channels.json")
	os.WriteFile(path, b, 0644)
}

func (s *AtChannelStore) Load() {
	path := filepath.Join(s.configDir, "at-channels.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var data struct {
		Initiators map[string][]string `json:"initiators"`
	}
	if json.Unmarshal(b, &data) != nil {
		return
	}
	s.mu.Lock()
	for init, tgts := range data.Initiators {
		if s.initiators[init] == nil {
			s.initiators[init] = make(map[string]bool)
		}
		for _, t := range tgts {
			s.initiators[init][t] = true
			if s.targets[t] == nil {
				s.targets[t] = make(map[string]bool)
			}
			s.targets[t][init] = true
		}
	}
	s.mu.Unlock()
	logger.Info(fmt.Sprintf("AtChannels loaded: %d initiators", len(data.Initiators)))
}

func (s *AtChannelStore) AppendBuffer(initiator, text string) {
	s.mu.Lock()
	s.buffers[initiator] = append(s.buffers[initiator], text)
	s.mu.Unlock()
}

func (s *AtChannelStore) FlushBufferEntries(initiator string) []string {
	s.mu.Lock()
	items := s.buffers[initiator]
	delete(s.buffers, initiator)
	s.mu.Unlock()
	return items
}
