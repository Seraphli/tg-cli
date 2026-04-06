package stores

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/Seraphli/tg-cli/internal/logger"
)

// CronJob represents a scheduled task.
type CronJob struct {
	ID         string    `json:"id"`
	Name       string    `json:"name,omitempty"`
	Mode       string    `json:"mode"`
	Schedule   string    `json:"schedule"`
	Once       bool      `json:"once"`
	Prompt     string    `json:"prompt"`
	AgentName  string    `json:"agent_name,omitempty"`
	TmuxTarget string    `json:"tmux_target,omitempty"`
	CWD        string    `json:"cwd,omitempty"`
	SessionID  string    `json:"session_id,omitempty"`
	MaxTurns   int       `json:"max_turns,omitempty"`
	NoHeader   bool      `json:"no_header,omitempty"`
	Paused     bool      `json:"paused,omitempty"`
	Fresh      bool      `json:"fresh,omitempty"`
	LastRun    time.Time `json:"last_run,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// CronJobStore stores cron jobs with persistence.
type CronJobStore struct {
	mu        sync.RWMutex
	jobs      map[string]*CronJob
	configDir string
}

// NewCronJobStore creates an empty CronJobStore with the given config directory.
func NewCronJobStore(configDir string) *CronJobStore {
	return &CronJobStore{
		jobs:      make(map[string]*CronJob),
		configDir: configDir,
	}
}

// Add inserts a new cron job, returning an error if the name already exists.
func (cs *CronJobStore) Add(job *CronJob) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if job.Name != "" {
		for _, j := range cs.jobs {
			if j.Name == job.Name {
				return fmt.Errorf("name '%s' already exists", job.Name)
			}
		}
	}
	cs.jobs[job.ID] = job
	cs.saveLocked()
	return nil
}

// Remove deletes a job by ID or name, returning whether it was found.
func (cs *CronJobStore) Remove(idOrName string) bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if _, ok := cs.jobs[idOrName]; ok {
		delete(cs.jobs, idOrName)
		cs.saveLocked()
		return true
	}
	for id, j := range cs.jobs {
		if j.Name == idOrName {
			delete(cs.jobs, id)
			cs.saveLocked()
			return true
		}
	}
	return false
}

// FindByName returns a copy of the cron job with the given name, or nil.
func (cs *CronJobStore) FindByName(name string) *CronJob {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	for _, j := range cs.jobs {
		if j.Name == name {
			cp := *j
			return &cp
		}
	}
	return nil
}

// FindByIDOrName returns a copy of the cron job by ID or name, or nil.
func (cs *CronJobStore) FindByIDOrName(idOrName string) *CronJob {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	if j, ok := cs.jobs[idOrName]; ok {
		cp := *j
		return &cp
	}
	for _, j := range cs.jobs {
		if j.Name == idOrName {
			cp := *j
			return &cp
		}
	}
	return nil
}

// Update applies string updates to an existing job by ID or name.
func (cs *CronJobStore) Update(idOrName string, updates map[string]string) bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	var job *CronJob
	if j, ok := cs.jobs[idOrName]; ok {
		job = j
	} else {
		for _, j := range cs.jobs {
			if j.Name == idOrName {
				job = j
				break
			}
		}
	}
	if job == nil {
		return false
	}
	if v, ok := updates["prompt"]; ok {
		job.Prompt = v
	}
	if v, ok := updates["schedule"]; ok {
		job.Schedule = v
	}
	if v, ok := updates["agent_name"]; ok {
		job.AgentName = v
	}
	if v, ok := updates["name"]; ok {
		for _, j := range cs.jobs {
			if j.ID != job.ID && j.Name == v {
				return false
			}
		}
		job.Name = v
	}
	if v, ok := updates["max_turns"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			job.MaxTurns = n
		}
	}
	if v, ok := updates["paused"]; ok {
		if b, err := strconv.ParseBool(v); err == nil {
			job.Paused = b
		}
	}
	cs.saveLocked()
	return true
}

// Get returns the job pointer for the given ID (not a copy).
func (cs *CronJobStore) Get(id string) *CronJob {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.jobs[id]
}

// All returns copies of all cron jobs.
func (cs *CronJobStore) All() []*CronJob {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	result := make([]*CronJob, 0, len(cs.jobs))
	for _, j := range cs.jobs {
		cp := *j
		result = append(result, &cp)
	}
	return result
}

// UpdateLastRun records the last run time for a job.
func (cs *CronJobStore) UpdateLastRun(id string, t time.Time) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if j, ok := cs.jobs[id]; ok {
		j.LastRun = t
		cs.saveLocked()
	}
}

// UpdateSessionID records the session ID for a job.
func (cs *CronJobStore) UpdateSessionID(id, sessionID string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if j, ok := cs.jobs[id]; ok {
		j.SessionID = sessionID
		cs.saveLocked()
	}
}

func (cs *CronJobStore) saveLocked() {
	jobs := make([]*CronJob, 0, len(cs.jobs))
	for _, j := range cs.jobs {
		jobs = append(jobs, j)
	}
	data, err := json.MarshalIndent(jobs, "", "  ")
	if err != nil {
		return
	}
	path := filepath.Join(cs.configDir, "cron-jobs.json")
	os.WriteFile(path, data, 0644)
}

// Load restores cron jobs from disk.
func (cs *CronJobStore) Load() {
	path := filepath.Join(cs.configDir, "cron-jobs.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var jobs []*CronJob
	if err := json.Unmarshal(data, &jobs); err != nil {
		return
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for _, j := range jobs {
		cs.jobs[j.ID] = j
	}
	logger.Info(fmt.Sprintf("Loaded %d cron jobs", len(jobs)))
}
