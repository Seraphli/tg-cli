package archive

import (
	"os"
	"path/filepath"
	"testing"
)

func newTestArchive(t *testing.T) (*Archive, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "messages.db")
	a, err := New(dbPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { a.Close() })
	return a, dbPath
}

// A send reserved then completed, followed by an edit, shares one message id; the operations carry the
// right op/rich/content/text_len; and the message row has the final tg_msg_id + chat_id.
func TestReserveCompleteRecordEdit(t *testing.T) {
	a, _ := newTestArchive(t)
	id, err := a.ReserveSend(100)
	if err != nil {
		t.Fatalf("ReserveSend: %v", err)
	}
	if id <= 0 {
		t.Fatalf("ReserveSend returned non-positive id %d", id)
	}
	if err := a.CompleteSend(id, 100, 555, true, "🆔 #1 hello", "cc-uuid-abc"); err != nil {
		t.Fatalf("CompleteSend: %v", err)
	}
	lookup, err := a.LookupOrCreate(100, 555)
	if err != nil {
		t.Fatalf("LookupOrCreate: %v", err)
	}
	if lookup != id {
		t.Fatalf("LookupOrCreate on existing (chat,tg) returned %d, want %d", lookup, id)
	}
	if err := a.RecordEdit(id, false, "edited body", ""); err != nil {
		t.Fatalf("RecordEdit: %v", err)
	}
	// messages row: final tg_msg_id + chat_id + cc_message_id set by CompleteSend.
	var chatID, tgMsgID int64
	var ccMsgID string
	if err := a.db.QueryRow(`SELECT chat_id, tg_msg_id, cc_message_id FROM messages WHERE id=?`, id).Scan(&chatID, &tgMsgID, &ccMsgID); err != nil {
		t.Fatalf("query messages: %v", err)
	}
	if chatID != 100 || tgMsgID != 555 {
		t.Fatalf("messages row chat_id=%d tg_msg_id=%d, want 100/555", chatID, tgMsgID)
	}
	if ccMsgID != "cc-uuid-abc" {
		t.Fatalf("messages row cc_message_id=%q, want cc-uuid-abc", ccMsgID)
	}
	// operations: one send (rich=1, content has the id line) + one edit (rich=0), both under id.
	rows, err := a.db.Query(`SELECT op, rich, text_len, content FROM operations WHERE msg_id=? ORDER BY op_id`, id)
	if err != nil {
		t.Fatalf("query operations: %v", err)
	}
	defer rows.Close()
	type op struct {
		op      string
		rich    int
		textLen int
		content string
	}
	var got []op
	for rows.Next() {
		var o op
		if err := rows.Scan(&o.op, &o.rich, &o.textLen, &o.content); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, o)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 operations, got %d", len(got))
	}
	if got[0].op != "send" || got[0].rich != 1 || got[0].content != "🆔 #1 hello" {
		t.Errorf("send op mismatch: %+v", got[0])
	}
	if got[0].textLen != len([]rune("🆔 #1 hello")) {
		t.Errorf("send text_len=%d, want %d", got[0].textLen, len([]rune("🆔 #1 hello")))
	}
	if got[1].op != "edit" || got[1].rich != 0 || got[1].content != "edited body" {
		t.Errorf("edit op mismatch: %+v", got[1])
	}
}

// LookupOrCreate mints a new id for a fresh (chat,tg) and returns the SAME id when called again.
func TestLookupOrCreateIdempotent(t *testing.T) {
	a, _ := newTestArchive(t)
	first, err := a.LookupOrCreate(7, 42)
	if err != nil {
		t.Fatalf("LookupOrCreate first: %v", err)
	}
	second, err := a.LookupOrCreate(7, 42)
	if err != nil {
		t.Fatalf("LookupOrCreate second: %v", err)
	}
	if first != second {
		t.Fatalf("LookupOrCreate not idempotent: %d != %d", first, second)
	}
	other, err := a.LookupOrCreate(7, 43)
	if err != nil {
		t.Fatalf("LookupOrCreate other: %v", err)
	}
	if other == first {
		t.Fatalf("different (chat,tg) should mint a different id, both = %d", first)
	}
}

// M2: CompleteSend with a chatID different from ReserveSend's updates messages.chat_id, and a later
// LookupOrCreate(newChatID, tgMsgID) finds the same id (no duplicate) — the group-migration case.
func TestCompleteSendMigratesChat(t *testing.T) {
	a, _ := newTestArchive(t)
	id, err := a.ReserveSend(-100) // reserved under the old (pre-migration) chat id
	if err != nil {
		t.Fatalf("ReserveSend: %v", err)
	}
	if err := a.CompleteSend(id, -200, 900, false, "body", ""); err != nil { // final chat id differs
		t.Fatalf("CompleteSend: %v", err)
	}
	var chatID int64
	if err := a.db.QueryRow(`SELECT chat_id FROM messages WHERE id=?`, id).Scan(&chatID); err != nil {
		t.Fatalf("query: %v", err)
	}
	if chatID != -200 {
		t.Fatalf("chat_id not migrated: got %d want -200", chatID)
	}
	same, err := a.LookupOrCreate(-200, 900)
	if err != nil {
		t.Fatalf("LookupOrCreate: %v", err)
	}
	if same != id {
		t.Fatalf("edit after migration minted a duplicate: got %d want %d", same, id)
	}
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 messages row after migration, got %d", count)
	}
}

// A nil *Archive is a no-op for every method — the sender's single top-level nil guard is enough.
func TestNilArchiveNoOp(t *testing.T) {
	var a *Archive
	if id, err := a.ReserveSend(1); id != 0 || err != nil {
		t.Errorf("nil ReserveSend = (%d,%v), want (0,nil)", id, err)
	}
	if id, err := a.LookupOrCreate(1, 2); id != 0 || err != nil {
		t.Errorf("nil LookupOrCreate = (%d,%v), want (0,nil)", id, err)
	}
	if err := a.CompleteSend(1, 1, 2, false, "x", ""); err != nil {
		t.Errorf("nil CompleteSend = %v, want nil", err)
	}
	if err := a.RecordEdit(1, false, "x", ""); err != nil {
		t.Errorf("nil RecordEdit = %v, want nil", err)
	}
	if err := a.RecordHookEvent("Stop", "s", "m", "t", "p", []byte("{}")); err != nil {
		t.Errorf("nil RecordHookEvent = %v, want nil", err)
	}
	if err := a.Close(); err != nil {
		t.Errorf("nil Close = %v, want nil", err)
	}
}

// RecordHookEvent stores the payload + identity fields, and hook_events joins to the messages row that
// carries the same CC message_id (cc_message_id), the Change-2 correlation.
func TestRecordHookEventAndJoin(t *testing.T) {
	a, _ := newTestArchive(t)
	// A stream send: the messages row gets cc_message_id = the CC UUID.
	id, err := a.ReserveSend(500)
	if err != nil {
		t.Fatalf("ReserveSend: %v", err)
	}
	if err := a.CompleteSend(id, 500, 777, true, "hello", "cc-uuid-xyz"); err != nil {
		t.Fatalf("CompleteSend: %v", err)
	}
	// Two hook events carrying the same CC UUID.
	if err := a.RecordHookEvent("MessageDisplay", "sess-1", "cc-uuid-xyz", "turn-9", "", []byte(`{"session_id":"sess-1","message_id":"cc-uuid-xyz"}`)); err != nil {
		t.Fatalf("RecordHookEvent MD: %v", err)
	}
	if err := a.RecordHookEvent("Stop", "sess-1", "cc-uuid-xyz", "turn-9", "prompt-3", []byte(`{"session_id":"sess-1"}`)); err != nil {
		t.Fatalf("RecordHookEvent Stop: %v", err)
	}
	// Field round-trip on one row.
	var name, sid, mid, tid, pid, payload string
	if err := a.db.QueryRow(`SELECT hook_event_name, session_id, message_id, turn_id, prompt_id, payload FROM hook_events WHERE hook_event_name='Stop'`).
		Scan(&name, &sid, &mid, &tid, &pid, &payload); err != nil {
		t.Fatalf("query hook_events: %v", err)
	}
	if name != "Stop" || sid != "sess-1" || mid != "cc-uuid-xyz" || tid != "turn-9" || pid != "prompt-3" || payload != `{"session_id":"sess-1"}` {
		t.Fatalf("hook_events row mismatch: name=%q sid=%q mid=%q tid=%q pid=%q payload=%q", name, sid, mid, tid, pid, payload)
	}
	// Join: hook_events.message_id = messages.cc_message_id → the two events link to the one message.
	var joined int
	if err := a.db.QueryRow(
		`SELECT COUNT(*) FROM hook_events h JOIN messages m ON h.message_id = m.cc_message_id WHERE m.id=?`, id).Scan(&joined); err != nil {
		t.Fatalf("join query: %v", err)
	}
	if joined != 2 {
		t.Fatalf("join returned %d rows, want 2 (both hook events link to message id=%d)", joined, id)
	}
}

// RecordEdit backfills cc_message_id when the message row has none yet (an assistant message whose first
// archived op is an edit).
func TestRecordEditBackfillsCCMessageID(t *testing.T) {
	a, _ := newTestArchive(t)
	id, err := a.LookupOrCreate(600, 888) // row minted with cc_message_id=''
	if err != nil {
		t.Fatalf("LookupOrCreate: %v", err)
	}
	if err := a.RecordEdit(id, true, "edited", "cc-late"); err != nil {
		t.Fatalf("RecordEdit: %v", err)
	}
	var cc string
	if err := a.db.QueryRow(`SELECT cc_message_id FROM messages WHERE id=?`, id).Scan(&cc); err != nil {
		t.Fatalf("query: %v", err)
	}
	if cc != "cc-late" {
		t.Fatalf("cc_message_id backfill = %q, want cc-late", cc)
	}
}

// The main DB file and its WAL/SHM sidecars are all mode 0600 after a write (sidecars must not leak
// message content at the umask default).
func TestFilePermissions0600(t *testing.T) {
	a, dbPath := newTestArchive(t)
	// Force a WAL write so the -wal/-shm sidecars exist before we stat them.
	if _, err := a.ReserveSend(1); err != nil {
		t.Fatalf("ReserveSend: %v", err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(dbPath + suffix)
		if err != nil {
			t.Fatalf("stat %s: %v", dbPath+suffix, err)
		}
		if perm := info.Mode().Perm(); perm != 0600 {
			t.Errorf("%s mode = %o, want 0600", dbPath+suffix, perm)
		}
	}
}

// A constructor failure (e.g. a directory in place of the db file) must not leak a db handle; New
// returns an error and no usable Archive.
func TestNewErrorPath(t *testing.T) {
	dir := t.TempDir()
	// A path that cannot be opened as a regular file: point at the directory itself.
	if _, err := New(dir); err == nil {
		t.Fatalf("New on a directory path should error")
	}
}
