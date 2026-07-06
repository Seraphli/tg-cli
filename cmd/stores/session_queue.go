package stores

import (
	"sync"

	"github.com/Seraphli/tg-cli/internal/logger"
)

type sessionEventJob struct {
	label   string
	handler func() error
	done    chan error
}

type sessionWorker struct {
	queue chan *sessionEventJob
}

type SessionEventStore struct {
	mu      sync.Mutex
	workers map[string]*sessionWorker
}

func NewSessionEventStore() *SessionEventStore {
	return &SessionEventStore{
		workers: make(map[string]*sessionWorker),
	}
}

func (s *SessionEventStore) getOrCreate(sessionID string) *sessionWorker {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w, ok := s.workers[sessionID]; ok {
		return w
	}
	w := &sessionWorker{queue: make(chan *sessionEventJob, 128)}
	s.workers[sessionID] = w
	go s.run(sessionID, w)
	return w
}

func (s *SessionEventStore) run(sessionID string, w *sessionWorker) {
	for job := range w.queue {
		err := job.handler()
		if err != nil {
			logger.Debug("SessionEventStore: session=" + sessionID + " job=" + job.label + " err=" + err.Error())
		}
		if job.done != nil {
			job.done <- err
			close(job.done)
		}
	}
}

func (s *SessionEventStore) Dispatch(sessionID, label string, handler func() error) error {
	if sessionID == "" {
		return handler()
	}
	w := s.getOrCreate(sessionID)
	done := make(chan error, 1)
	w.queue <- &sessionEventJob{label: label, handler: handler, done: done}
	return <-done
}

func (s *SessionEventStore) DispatchAsync(sessionID, label string, handler func() error) {
	if sessionID == "" {
		handler()
		return
	}
	w := s.getOrCreate(sessionID)
	w.queue <- &sessionEventJob{label: label, handler: handler}
}

// TryDispatchAsync attempts a nonblocking enqueue to the session's worker queue.
// Returns true if the job was accepted, false if the channel is full.
// Falls back to sessionID=="" behavior (inline run) if sessionID is empty.
func (s *SessionEventStore) TryDispatchAsync(sessionID, label string, handler func() error) bool {
	if sessionID == "" {
		handler()
		return true
	}
	w := s.getOrCreate(sessionID)
	select {
	case w.queue <- &sessionEventJob{label: label, handler: handler}:
		return true
	default:
		return false
	}
}

// QueueDepth returns the number of queued (not-yet-started) jobs for a session's worker, 0 if none.
// Observability only (logged at the PreToolUse bounded-wait entry).
func (s *SessionEventStore) QueueDepth(sessionID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w, ok := s.workers[sessionID]; ok {
		return len(w.queue)
	}
	return 0
}
