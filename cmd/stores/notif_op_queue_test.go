package stores

import (
	"fmt"
	"testing"
	"time"
)

func TestNotifOpQueue_AnswerAfterSend(t *testing.T) {
	q := NewNotifOpQueue()
	defer q.Close()

	uuid := "test-uuid-1"
	sendDone := make(chan struct{})

	// Enqueue SEND
	q.TryEnqueue(NotifOp{
		Type: OpSEND,
		UUID: uuid,
		SendFunc: func(frozen bool, frozenLabel string) SendResult {
			return SendResult{MsgID: 42, ChatID: 100}
		},
		PostSend: func(result SendResult) {
			close(sendDone)
		},
	})

	<-sendDone

	// After send, msgIDs should exist and be Ready
	msgID, chatID, _, ok := q.GetReadyMsgID(uuid)
	if !ok || msgID != 42 || chatID != 100 {
		t.Fatalf("expected Ready msgID=42 chatID=100, got ok=%v msgID=%d chatID=%d", ok, msgID, chatID)
	}

	// Enqueue EDIT — should execute and self-clean
	editDone := make(chan struct{})
	q.TryEnqueue(NotifOp{
		Type: OpEDIT,
		UUID: uuid,
		EditFunc: func(msgID int, chatID int64, msgText string) {
			if msgID != 42 || chatID != 100 {
				t.Errorf("EDIT got wrong coords: msgID=%d chatID=%d", msgID, chatID)
			}
			close(editDone)
		},
	})

	<-editDone
	time.Sleep(50 * time.Millisecond)

	// After EDIT self-clean, msgIDs should be gone
	_, _, _, ok = q.GetMsgID(uuid)
	if ok {
		t.Fatal("expected msgIDs to be cleaned after EDIT")
	}
}

func TestNotifOpQueue_Coalesce(t *testing.T) {
	q := NewNotifOpQueue()
	defer q.Close()

	uuid := "test-coalesce"
	editExecuted := make(chan struct{})
	sendDone := make(chan struct{})

	// Enqueue EDIT before SEND (answer-before-send path).
	// The EditFunc IS called by the SEND handler (coalesced), not via handleEDIT separately.
	q.TryEnqueue(NotifOp{
		Type:        OpEDIT,
		UUID:        uuid,
		FreezeLabel: "✅ Custom reply",
		EditFunc: func(msgID int, chatID int64, msgText string) {
			close(editExecuted)
		},
	})

	// Enqueue SEND that will check coalesce parameters
	q.TryEnqueue(NotifOp{
		Type: OpSEND,
		UUID: uuid,
		SendFunc: func(frozen bool, frozenLabel string) SendResult {
			if !frozen || frozenLabel != "✅ Custom reply" {
				t.Errorf("expected coalesced frozen=true label='✅ Custom reply', got frozen=%v label=%s", frozen, frozenLabel)
			}
			return SendResult{MsgID: 42, ChatID: 100}
		},
		PostSend: func(result SendResult) {
			close(sendDone)
		},
	})

	<-sendDone
	// EditFunc must have been called as part of coalesce (drained by SEND handler)
	select {
	case <-editExecuted:
		// ok
	default:
		t.Fatal("EditFunc should have been called via coalesce in SEND handler")
	}
}

func TestNotifOpQueue_SENDFailure(t *testing.T) {
	q := NewNotifOpQueue()
	defer q.Close()

	uuid := "test-send-fail"
	postSendDone := make(chan struct{})

	q.TryEnqueue(NotifOp{
		Type: OpSEND,
		UUID: uuid,
		SendFunc: func(frozen bool, frozenLabel string) SendResult {
			return SendResult{MainErr: fmt.Errorf("network error")}
		},
		PostSend: func(result SendResult) {
			if result.MainErr == nil {
				t.Error("expected MainErr")
			}
			close(postSendDone)
		},
	})

	<-postSendDone
	time.Sleep(50 * time.Millisecond)

	// After SEND failure, uuid should be closed (tombstone)
	_, _, _, ok := q.GetMsgID(uuid)
	if ok {
		t.Fatal("expected msgIDs cleaned after SEND failure")
	}
}

func TestNotifOpQueue_NonBlocking257th(t *testing.T) {
	q := NewNotifOpQueue()
	defer q.Close()

	// Fill the channel with 256 CLEANUP ops (they're simple)
	// Note: the worker will process them, so we need to be fast
	for i := 0; i < 256; i++ {
		ok := q.TryEnqueue(NotifOp{
			Type: OpCLEANUP,
			UUID: fmt.Sprintf("fill-%d", i),
		})
		if !ok {
			// Channel might not be full yet if worker is fast
			continue
		}
	}

	// 257th SEND should not block (returns false if channel full)
	ok := q.TryEnqueue(NotifOp{
		Type: OpSEND,
		UUID: "overflow",
		SendFunc: func(bool, string) SendResult { return SendResult{} },
	})
	// ok can be true (worker drained fast) or false (channel full) — both are acceptable
	_ = ok
}

func TestNotifOpQueue_EDITFullSyncFallback(t *testing.T) {
	q := NewNotifOpQueue()
	defer q.Close()

	uuid := "test-edit-full"

	// Manually store a Ready msgID
	q.mu.Lock()
	q.msgIDs[uuid] = &msgIDEntry{MsgID: 42, ChatID: 100, MsgText: "hello", Ready: true}
	q.mu.Unlock()

	// Block the worker with a slow SEND (worker reads it and waits)
	blocker := make(chan struct{})
	q.TryEnqueue(NotifOp{
		Type: OpSEND,
		UUID: "blocker",
		SendFunc: func(bool, string) SendResult {
			<-blocker
			return SendResult{MsgID: 1, ChatID: 1}
		},
		PostSend: func(SendResult) {},
	})

	// The worker has now dequeued the blocker SEND and is blocked.
	// Fill ALL 256 channel slots to force sync fallback on the EDIT.
	for i := 0; i < 256; i++ {
		q.TryEnqueue(NotifOp{
			Type: OpCLEANUP,
			UUID: fmt.Sprintf("fill-%d", i),
		})
	}

	// Now EDIT should trigger sync fallback because channel is full
	editExecuted := false
	ok := q.TryEnqueue(NotifOp{
		Type: OpEDIT,
		UUID: uuid,
		EditFunc: func(msgID int, chatID int64, msgText string) {
			if msgID != 42 {
				t.Errorf("sync fallback got wrong msgID: %d", msgID)
			}
			editExecuted = true
		},
	})

	if !ok {
		t.Fatal("TryEnqueue(EDIT) should always return true")
	}
	if !editExecuted {
		t.Fatal("sync fallback should have executed EditFunc")
	}

	close(blocker) // unblock worker
}

func TestNotifOpQueue_ReadyFlag(t *testing.T) {
	q := NewNotifOpQueue()
	defer q.Close()

	uuid := "test-ready"

	// Store not-ready entry
	q.mu.Lock()
	q.msgIDs[uuid] = &msgIDEntry{MsgID: 42, ChatID: 100, Ready: false}
	q.mu.Unlock()

	// GetReadyMsgID should fail
	_, _, _, ok := q.GetReadyMsgID(uuid)
	if ok {
		t.Fatal("expected not ready")
	}

	// GetMsgID should succeed (ignores Ready)
	msgID, _, _, ok := q.GetMsgID(uuid)
	if !ok || msgID != 42 {
		t.Fatal("GetMsgID should work regardless of Ready")
	}

	// MarkReady
	q.MarkReady(uuid)
	_, _, _, ok = q.GetReadyMsgID(uuid)
	if !ok {
		t.Fatal("expected ready after MarkReady")
	}
}

func TestNotifOpQueue_ClosedTombstone(t *testing.T) {
	q := NewNotifOpQueue()
	defer q.Close()

	uuid := "test-closed"

	// Cleanup creates tombstone
	q.Cleanup(uuid)

	// EDIT should be discarded
	editCalled := false
	ok := q.TryEnqueue(NotifOp{
		Type: OpEDIT,
		UUID: uuid,
		EditFunc: func(int, int64, string) {
			editCalled = true
		},
	})
	if !ok {
		t.Fatal("EDIT+closed should return true (discarded)")
	}
	if editCalled {
		t.Fatal("EDIT should be discarded due to closed tombstone")
	}
}

func TestNotifOpQueue_SweepThrottle(t *testing.T) {
	q := NewNotifOpQueue()
	defer q.Close()

	// Add old closed entry
	q.mu.Lock()
	q.closed["old"] = time.Now().Unix() - 120 // 2 minutes old
	q.lastSweep = time.Now().Unix() - 15      // last sweep 15s ago (>10s threshold)
	q.mu.Unlock()

	// Cleanup triggers markClosed which sweeps
	q.Cleanup("trigger")

	time.Sleep(50 * time.Millisecond)

	q.mu.Lock()
	_, oldExists := q.closed["old"]
	q.mu.Unlock()

	if oldExists {
		t.Fatal("expected old closed entry to be swept")
	}
}

func TestNotifOpQueue_ReconnectGetMsgID(t *testing.T) {
	q := NewNotifOpQueue()
	defer q.Close()

	uuid := "test-reconnect"

	// Store not-ready (SEND in progress)
	q.mu.Lock()
	q.msgIDs[uuid] = &msgIDEntry{MsgID: 42, ChatID: 100, Ready: false}
	q.mu.Unlock()

	// GetMsgID should return it regardless of Ready (for reconnect)
	msgID, chatID, _, ok := q.GetMsgID(uuid)
	if !ok || msgID != 42 || chatID != 100 {
		t.Fatalf("GetMsgID should work for reconnect: ok=%v msgID=%d chatID=%d", ok, msgID, chatID)
	}
}
