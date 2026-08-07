package archive

import (
	"database/sql"
	"fmt"
	"os"
	"time"

	_ "modernc.org/sqlite"
)

// Archive records the text-message send/edit operations routed through the Retry* helpers into an
// independent SQLite database. All methods are nil-safe: a nil *Archive is a no-op (returns 0/nil), so
// callers need only a single top-level nil guard. Methods RETURN errors and never block the send — the
// caller logs them as warnings.
type Archive struct {
	db *sql.DB
}

// migrations create the schema. Split into individual statements so the migration does not depend on a
// driver's multi-statement Exec support. cc_message_id links a message to the CC assistant-message UUID
// (only stream/assistant sends carry one), the join key to hook_events.message_id.
var migrations = []string{
	`CREATE TABLE IF NOT EXISTS messages (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		chat_id       INTEGER NOT NULL,
		tg_msg_id     INTEGER NOT NULL DEFAULT 0,
		created_at    TEXT    NOT NULL,
		cc_message_id TEXT    NOT NULL DEFAULT ''
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS ux_messages_chat_tg ON messages(chat_id, tg_msg_id) WHERE tg_msg_id != 0`,
	`CREATE TABLE IF NOT EXISTS operations (
		op_id    INTEGER PRIMARY KEY AUTOINCREMENT,
		msg_id   INTEGER NOT NULL,
		op       TEXT    NOT NULL,
		ts       TEXT    NOT NULL,
		rich     INTEGER NOT NULL,
		text_len INTEGER NOT NULL,
		content  TEXT    NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS ix_operations_msg ON operations(msg_id)`,
	`CREATE TABLE IF NOT EXISTS hook_events (
		event_id        INTEGER PRIMARY KEY AUTOINCREMENT,
		hook_event_name TEXT NOT NULL,
		ts              TEXT NOT NULL,
		session_id      TEXT NOT NULL,
		message_id      TEXT NOT NULL,
		turn_id         TEXT NOT NULL,
		prompt_id       TEXT NOT NULL,
		payload         TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS ix_hook_events_msg ON hook_events(message_id)`,
	`CREATE INDEX IF NOT EXISTS ix_hook_events_session ON hook_events(session_id)`,
}

// New opens (creating if needed) the SQLite archive at dbPath. It pre-creates the main DB file at 0600
// BEFORE sql.Open so the WAL -wal/-shm sidecars (which hold message content) inherit 0600 rather than
// the umask default. On any post-Open failure the db handle is closed before returning.
func New(dbPath string) (*Archive, error) {
	f, err := os.OpenFile(dbPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("archive: create db file: %w", err)
	}
	f.Close()
	if err := os.Chmod(dbPath, 0600); err != nil {
		return nil, fmt.Errorf("archive: chmod db file: %w", err)
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, fmt.Errorf("archive: open db: %w", err)
	}
	db.SetMaxOpenConns(1) // single serialized writer — removes SQLITE_BUSY races
	for _, stmt := range migrations {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("archive: migrate schema: %w", err)
		}
	}
	// A messages.db created before cc_message_id existed keeps its old schema (CREATE TABLE IF NOT EXISTS is
	// a no-op on the existing table), so add the column idempotently on pre-existing DBs.
	has, err := columnExists(db, "messages", "cc_message_id")
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("archive: inspect schema: %w", err)
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE messages ADD COLUMN cc_message_id TEXT NOT NULL DEFAULT ''`); err != nil {
			db.Close()
			return nil, fmt.Errorf("archive: add cc_message_id: %w", err)
		}
	}
	return &Archive{db: db}, nil
}

// columnExists reports whether table has a column of the given name (via PRAGMA table_info).
func columnExists(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func runeLen(s string) int {
	return len([]rune(s))
}

// nowRFC3339 is the created_at stamp (second precision).
func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// nowMillis is the operations.ts stamp (millisecond precision).
func nowMillis() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// ReserveSend inserts a message row with tg_msg_id=0 and returns its AUTOINCREMENT id (the tg-cli
// message ID) BEFORE the header is finalized — solving the chicken-and-egg where the TG msg_id is not
// known until after the send.
func (a *Archive) ReserveSend(chatID int64) (int64, error) {
	if a == nil {
		return 0, nil
	}
	res, err := a.db.Exec(`INSERT INTO messages(chat_id, tg_msg_id, created_at) VALUES(?, 0, ?)`, chatID, nowRFC3339())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// CompleteSend fills in the message's final chat_id + tg_msg_id (+ cc_message_id when known) and records
// the send operation, in ONE transaction. It takes the FINAL chatID (a GroupError migration mutates the
// recipient) so a later edit resolves the same id via LookupOrCreate instead of minting a duplicate.
// ccMessageID is the CC assistant-message UUID for a stream send ("" for non-assistant sends).
func (a *Archive) CompleteSend(id, chatID, tgMsgID int64, rich bool, content, ccMessageID string) error {
	if a == nil {
		return nil
	}
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE messages SET chat_id=?, tg_msg_id=?, cc_message_id=? WHERE id=?`, chatID, tgMsgID, ccMessageID, id); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`INSERT INTO operations(msg_id, op, ts, rich, text_len, content) VALUES(?, 'send', ?, ?, ?, ?)`,
		id, nowMillis(), boolToInt(rich), runeLen(content), content); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// LookupOrCreate returns the tg-cli id for (chatID, tgMsgID), creating a message row if none exists. The
// INSERT ... ON CONFLICT DO NOTHING + SELECT is atomic against the partial-unique race (two concurrent
// edits of a not-yet-archived message). Called by EDIT paths; idempotent by (chat_id, tg_msg_id).
func (a *Archive) LookupOrCreate(chatID, tgMsgID int64) (int64, error) {
	if a == nil {
		return 0, nil
	}
	if _, err := a.db.Exec(
		`INSERT INTO messages(chat_id, tg_msg_id, created_at) VALUES(?, ?, ?) ON CONFLICT(chat_id, tg_msg_id) WHERE tg_msg_id != 0 DO NOTHING`,
		chatID, tgMsgID, nowRFC3339()); err != nil {
		return 0, err
	}
	var id int64
	if err := a.db.QueryRow(`SELECT id FROM messages WHERE chat_id=? AND tg_msg_id=?`, chatID, tgMsgID).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

// RecordEdit records an edit operation on an existing message id. NOT called for a "message not
// modified" success (no content change → no operation). When ccMessageID is non-empty and the message
// row has none yet, it is backfilled — so an assistant message whose first archived op is an edit still
// links to hook_events.
func (a *Archive) RecordEdit(id int64, rich bool, content, ccMessageID string) error {
	if a == nil {
		return nil
	}
	if ccMessageID != "" {
		if _, err := a.db.Exec(`UPDATE messages SET cc_message_id=? WHERE id=? AND cc_message_id=''`, ccMessageID, id); err != nil {
			return err
		}
	}
	_, err := a.db.Exec(`INSERT INTO operations(msg_id, op, ts, rich, text_len, content) VALUES(?, 'edit', ?, ?, ?, ?)`,
		id, nowMillis(), boolToInt(rich), runeLen(content), content)
	return err
}

// RecordHookEvent stores a received CC hook payload (verbatim JSON) with its parsed identity fields.
// nil-safe; the caller logs the error and never blocks the hook.
func (a *Archive) RecordHookEvent(eventName, sessionID, messageID, turnID, promptID string, payload []byte) error {
	if a == nil {
		return nil
	}
	_, err := a.db.Exec(
		`INSERT INTO hook_events(hook_event_name, ts, session_id, message_id, turn_id, prompt_id, payload) VALUES(?, ?, ?, ?, ?, ?, ?)`,
		eventName, nowMillis(), sessionID, messageID, turnID, promptID, string(payload))
	return err
}

// RecordHookEventAt stores a received CC hook payload (verbatim JSON) with its parsed identity fields,
// stamping ts from the ingress arrivedAt at NANOSECOND precision. The /hook/ handler captures arrivedAt at
// ingress and calls this at the TOP of the queued handler (after the ordering-reservation enqueue), so ts is
// the authoritative, best-effort arrival-order key even though the AUTOINCREMENT event_id reflects
// drain-perturbed handler-execution order. The layout is FIXED-WIDTH (9-digit fraction, always trailing "Z")
// so TEXT ORDER BY ts is lexicographically == chronologically ordered — RFC3339Nano trims trailing zeros,
// which sorts a whole-second stamp AFTER its fractional neighbors ('Z' > '.') and breaks the ordering.
// nil-safe; the caller logs the error and never blocks the hook.
func (a *Archive) RecordHookEventAt(eventName, sessionID, messageID, turnID, promptID string, payload []byte, arrivedAt time.Time) error {
	if a == nil {
		return nil
	}
	_, err := a.db.Exec(
		`INSERT INTO hook_events(hook_event_name, ts, session_id, message_id, turn_id, prompt_id, payload) VALUES(?, ?, ?, ?, ?, ?, ?)`,
		eventName, arrivedAt.UTC().Format("2006-01-02T15:04:05.000000000Z07:00"), sessionID, messageID, turnID, promptID, string(payload))
	return err
}

func (a *Archive) Close() error {
	if a == nil {
		return nil
	}
	return a.db.Close()
}
