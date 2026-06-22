package stores

import (
	"sync"
	"time"

	"github.com/Seraphli/tg-cli/internal/logger"
	tele "gopkg.in/telebot.v3"
)

// SendResult holds the outcome of a TG send operation.
type SendResult struct {
	MsgID         int
	ChatID        int64
	TopicID       int
	MsgText       string
	MainErr       error
	SecondaryErrs []error
}

// NotifOpType distinguishes operation types in the queue.
type NotifOpType int

const (
	OpSEND    NotifOpType = iota
	OpEDIT
	OpCLEANUP
)

// NotifOp is a single operation submitted to the queue.
type NotifOp struct {
	Type         NotifOpType
	UUID         string
	FreezeLabel  string
	FrozenMarkup *tele.ReplyMarkup
	SendFunc     func(frozen bool, frozenLabel string) SendResult
	PostSend     func(result SendResult)
	EditFunc     func(msgID int, chatID int64, msgText string)
}

// msgIDEntry tracks a sent message's TG coordinates.
type msgIDEntry struct {
	MsgID   int
	ChatID  int64
	MsgText string
	Ready   bool
}

// pendEditEntry records a deferred EDIT for a uuid whose SEND hasn't completed yet.
type pendEditEntry struct {
	FreezeLabel  string
	FrozenMarkup *tele.ReplyMarkup
	EditFunc     func(msgID int, chatID int64, msgText string)
}

// NotifOpQueue manages async TG send/edit operations keyed by notification UUID.
type NotifOpQueue struct {
	mu        sync.Mutex
	ops       chan NotifOp
	msgIDs    map[string]*msgIDEntry
	pendEdits map[string]*pendEditEntry
	closed    map[string]int64
	lastSweep int64
	opCount   int
	done      chan struct{}
}

// NewNotifOpQueue creates and starts a NotifOpQueue with a buffered channel (cap 256).
func NewNotifOpQueue() *NotifOpQueue {
	q := &NotifOpQueue{
		ops:       make(chan NotifOp, 256),
		msgIDs:    make(map[string]*msgIDEntry),
		pendEdits: make(map[string]*pendEditEntry),
		closed:    make(map[string]int64),
		done:      make(chan struct{}),
	}
	go q.worker()
	return q
}

// markClosed sets the closed tombstone for a uuid. Caller MUST hold mu.
// Runs a throttled sweep (>10s since last) removing entries older than 60s.
func (q *NotifOpQueue) markClosed(uuid string) {
	q.closed[uuid] = time.Now().Unix()
	now := time.Now().Unix()
	if now-q.lastSweep > 10 {
		q.lastSweep = now
		for u, ts := range q.closed {
			if now-ts > 60 {
				delete(q.closed, u)
			}
		}
	}
}

// TryEnqueue submits an operation to the queue. Returns true if accepted.
// EDIT ops have special handling: closed→discard, Ready→channel or sync fallback, notReady→pendEdits.
// SEND/CLEANUP go to the channel directly.
func (q *NotifOpQueue) TryEnqueue(op NotifOp) bool {
	if op.Type == OpEDIT {
		q.mu.Lock()
		if _, ok := q.closed[op.UUID]; ok {
			q.mu.Unlock()
			logger.Debug("NotifOpQueue: EDIT discarded (closed tombstone) uuid=" + op.UUID)
			return true
		}
		entry, hasEntry := q.msgIDs[op.UUID]
		if hasEntry && entry.Ready {
			q.mu.Unlock()
			select {
			case q.ops <- op:
				return true
			default:
			}
			// Channel full — sync fallback inline
			msgID, chatID, msgText, ok := q.GetReadyMsgID(op.UUID)
			if ok && op.EditFunc != nil {
				op.EditFunc(msgID, chatID, msgText)
				q.mu.Lock()
				delete(q.msgIDs, op.UUID)
				q.mu.Unlock()
			}
			return true
		}
		// Not ready or unknown — record in pendEdits
		q.pendEdits[op.UUID] = &pendEditEntry{
			FreezeLabel:  op.FreezeLabel,
			FrozenMarkup: op.FrozenMarkup,
			EditFunc:     op.EditFunc,
		}
		q.mu.Unlock()
		return true
	}
	// SEND / CLEANUP
	select {
	case q.ops <- op:
		return true
	default:
		return false
	}
}

// GetMsgID returns the message ID for a uuid regardless of Ready state (used for reconnect).
func (q *NotifOpQueue) GetMsgID(uuid string) (msgID int, chatID int64, msgText string, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	e, exists := q.msgIDs[uuid]
	if !exists {
		return 0, 0, "", false
	}
	return e.MsgID, e.ChatID, e.MsgText, true
}

// GetReadyMsgID returns the message ID only if Ready (used for EDIT-full sync fallback).
func (q *NotifOpQueue) GetReadyMsgID(uuid string) (msgID int, chatID int64, msgText string, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	e, exists := q.msgIDs[uuid]
	if !exists || !e.Ready {
		return 0, 0, "", false
	}
	return e.MsgID, e.ChatID, e.MsgText, true
}

// MarkReady sets the Ready flag for a uuid.
func (q *NotifOpQueue) MarkReady(uuid string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if e, ok := q.msgIDs[uuid]; ok {
		e.Ready = true
	}
}

// SeedReadyMsgID inserts a pre-known message ID as already-ready (used for reconnect path).
func (q *NotifOpQueue) SeedReadyMsgID(uuid string, msgID int, chatID int64, msgText string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.msgIDs[uuid] = &msgIDEntry{
		MsgID:   msgID,
		ChatID:  chatID,
		MsgText: msgText,
		Ready:   true,
	}
}

// Cleanup synchronously marks a uuid as closed and deletes its state.
func (q *NotifOpQueue) Cleanup(uuid string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.markClosed(uuid)
	delete(q.msgIDs, uuid)
	delete(q.pendEdits, uuid)
}

// Close shuts down the worker goroutine.
func (q *NotifOpQueue) Close() {
	close(q.ops)
	<-q.done
}

// worker processes operations from the channel sequentially.
func (q *NotifOpQueue) worker() {
	defer close(q.done)
	for op := range q.ops {
		switch op.Type {
		case OpSEND:
			q.handleSEND(op)
		case OpEDIT:
			q.handleEDIT(op)
		case OpCLEANUP:
			q.handleCLEANUP(op)
		}
		q.opCount++
		if q.opCount%100 == 0 {
			q.mu.Lock()
			now := time.Now().Unix()
			if now-q.lastSweep > 10 {
				q.lastSweep = now
				for u, ts := range q.closed {
					if now-ts > 60 {
						delete(q.closed, u)
					}
				}
			}
			q.mu.Unlock()
		}
	}
}

func (q *NotifOpQueue) handleSEND(op NotifOp) {
	// Step 1: Check pendEdits for coalesce
	q.mu.Lock()
	var frozenLabel string
	var frozen bool
	if pe, ok := q.pendEdits[op.UUID]; ok {
		frozenLabel = pe.FreezeLabel
		frozen = true
	}
	q.mu.Unlock()

	// Step 2: Execute SendFunc
	result := op.SendFunc(frozen, frozenLabel)

	// Step 3: MainErr → cancel + cleanup
	if result.MainErr != nil {
		q.mu.Lock()
		q.markClosed(op.UUID)
		delete(q.pendEdits, op.UUID)
		delete(q.msgIDs, op.UUID)
		q.mu.Unlock()
		if op.PostSend != nil {
			op.PostSend(result)
		}
		return
	}

	// Step 4: Store msgIDs (Ready=false)
	q.mu.Lock()
	q.msgIDs[op.UUID] = &msgIDEntry{
		MsgID:   result.MsgID,
		ChatID:  result.ChatID,
		MsgText: result.MsgText,
		Ready:   false,
	}
	q.mu.Unlock()

	// Step 5: PostSend (stores + ndjson update via captured sendFrame)
	if op.PostSend != nil {
		op.PostSend(result)
	}

	// Step 6: MarkReady + take pendEdits
	q.mu.Lock()
	if e, ok := q.msgIDs[op.UUID]; ok {
		e.Ready = true
	}
	pe := q.pendEdits[op.UUID]
	delete(q.pendEdits, op.UUID)
	drained := pe != nil
	q.mu.Unlock()

	// Step 7: Execute drained EditFunc OUTSIDE mu
	if drained && pe.EditFunc != nil {
		e_msgID := result.MsgID
		e_chatID := result.ChatID
		pe.EditFunc(e_msgID, e_chatID, result.MsgText)
	}

	// Step 8: If drained, delete msgIDs
	if drained {
		q.mu.Lock()
		delete(q.msgIDs, op.UUID)
		q.mu.Unlock()
	}
}

func (q *NotifOpQueue) handleEDIT(op NotifOp) {
	q.mu.Lock()
	entry, exists := q.msgIDs[op.UUID]
	if !exists || !entry.Ready {
		// One-EDIT-per-uuid invariant: SEND may have already drained+executed it
		logger.Debug("NotifOpQueue: EDIT skipped (not found or not ready) uuid=" + op.UUID)
		q.mu.Unlock()
		return
	}
	msgID := entry.MsgID
	chatID := entry.ChatID
	msgText := entry.MsgText
	q.mu.Unlock()

	if op.EditFunc != nil {
		op.EditFunc(msgID, chatID, msgText)
	}

	q.mu.Lock()
	delete(q.msgIDs, op.UUID)
	q.mu.Unlock()
}

func (q *NotifOpQueue) handleCLEANUP(op NotifOp) {
	q.mu.Lock()
	q.markClosed(op.UUID)
	delete(q.msgIDs, op.UUID)
	delete(q.pendEdits, op.UUID)
	q.mu.Unlock()
}
