package cmd

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Seraphli/tg-cli/cmd/helpers"
	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/logger"
	"github.com/Seraphli/tg-cli/internal/markdown"
	tele "gopkg.in/telebot.v3"
)

// mailboxBodyMaxRunes is the fixed body budget per TG message for mailbox notifications.
// Leaves ~1000 runes headroom for the header block (from/to/timestamp/id/status/subject).
// Using a fixed value keeps send-time and edit-time chunk splits consistent.
const mailboxBodyMaxRunes = 3000

// buildMailboxChunks renders a mailbox notification into TG-ready message texts.
// firstLine is the notification title (e.g. "📤 Mail Sent", "📥 Mail Received", or "").
// subject and text are raw markdown; both are rendered to Telegram HTML.
// status is the pre-escaped status line appended to the END of the LAST chunk
// (e.g. "📫 Unread", "📭 Read — 2026-04-11 10:00"). Multi-chunk edit path only
// edits the last message; single-chunk path includes status in the one message.
// Returns one or more message texts; len > 1 means multi-message delivery with "📄 i/N" markers.
func buildMailboxChunks(firstLine, from, to, timestamp, msgID, subject, text, status string) []string {
	esc := markdown.EscapeHTML
	renderedSubject := markdown.RenderTelegramHTML(subject)
	renderedText := markdown.RenderTelegramHTML(text)
	var headerParts []string
	if firstLine != "" {
		headerParts = append(headerParts, firstLine)
	}
	headerParts = append(headerParts,
		"📤 From: "+esc(from),
		"📥 To: "+esc(to),
		"🕐 "+esc(timestamp),
		"🆔 "+esc(msgID),
		"━━━━━━━━━━",
		"Subject: "+renderedSubject,
		"━━━━━━━━━━",
	)
	header := strings.Join(headerParts, "\n") + "\n"
	statusSuffix := ""
	if status != "" {
		statusSuffix = "\n━━━━━━━━━━\n" + status
	}
	chunks := helpers.SplitBody(renderedText, mailboxBodyMaxRunes)
	if len(chunks) <= 1 {
		body := ""
		if len(chunks) == 1 {
			body = chunks[0]
		}
		return []string{header + body + statusSuffix}
	}
	result := make([]string, 0, len(chunks))
	for i, chunk := range chunks {
		marker := fmt.Sprintf("📄 %d/%d", i+1, len(chunks))
		var msg string
		if i == 0 {
			msg = header + marker + "\n" + chunk
		} else {
			msg = marker + "\n" + chunk
		}
		if i == len(chunks)-1 {
			msg += statusSuffix
		}
		result = append(result, msg)
	}
	return result
}

// sendMailboxChunks sends a slice of pre-built chunks as separate TG messages.
// Returns the full slice of sent messages (in send order) so callers can record
// every chunk's msg ID — the channel-post caller stores them into mailboxMessage.TGMsgIDs
// so editTGReadReceipt can edit the LAST chunk (which holds the status line at its end).
// All messages are sent with tele.ModeHTML parse mode.
func sendMailboxChunks(bot *tele.Bot, target tele.Recipient, chunks []string, extraOpts ...interface{}) ([]*tele.Message, error) {
	sentMsgs := make([]*tele.Message, 0, len(chunks))
	for _, chunk := range chunks {
		opts := append([]interface{}{tele.ModeHTML}, extraOpts...)
		sent, err := helpers.RetrySend(bot, target, chunk, opts...)
		if err != nil {
			return sentMsgs, err
		}
		sentMsgs = append(sentMsgs, sent)
	}
	return sentMsgs, nil
}

type mailboxMessage struct {
	ID        string    `json:"id"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	Subject   string    `json:"subject"`
	Text      string    `json:"text"`
	FileName  string    `json:"file_name,omitempty"`
	FileID    string    `json:"file_id,omitempty"`
	Timestamp time.Time `json:"timestamp"`
	Read      bool      `json:"read"`
	TGMsgIDs  []int     `json:"tg_msg_ids,omitempty"`
}

type mailboxStore struct {
	mu      sync.RWMutex
	waiters map[string][]chan []mailboxMessage
	dir     string
}

var mailbox = &mailboxStore{
	waiters: make(map[string][]chan []mailboxMessage),
}

// unreadPath returns the path to the agent's unread JSONL file.
func (ms *mailboxStore) unreadPath(name string) string {
	return filepath.Join(ms.dir, name+".unread.jsonl")
}

// readPath returns the path to the agent's read JSONL file.
func (ms *mailboxStore) readPath(name string) string {
	return filepath.Join(ms.dir, name+".read.jsonl")
}

// load creates the mailbox directory and migrates old data if needed.
func (ms *mailboxStore) load() {
	ms.dir = filepath.Join(config.GetConfigDir(), "mailbox")
	if err := os.MkdirAll(ms.dir, 0755); err != nil {
		logger.Error(fmt.Sprintf("Mailbox dir create failed: %v", err))
		return
	}
}

// appendToRead appends a single message as a JSON line to the agent's read JSONL file.
func (ms *mailboxStore) appendToRead(name string, msg mailboxMessage) {
	line, err := json.Marshal(msg)
	if err != nil {
		return
	}
	f, err := os.OpenFile(ms.readPath(name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	f.Write(line)
	f.Write([]byte("\n"))
	f.Close()
}

// appendToUnread appends a single message as a JSON line to the agent's unread JSONL file.
func (ms *mailboxStore) appendToUnread(name string, msg mailboxMessage) {
	line, err := json.Marshal(msg)
	if err != nil {
		return
	}
	f, err := os.OpenFile(ms.unreadPath(name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	f.Write(line)
	f.Write([]byte("\n"))
	f.Close()
	ms.rotate(name)
}

// rotate compresses the read JSONL file if it exceeds 100MB and starts a fresh one.
func (ms *mailboxStore) rotate(name string) {
	path := ms.readPath(name)
	info, err := os.Stat(path)
	if err != nil || info.Size() <= 100<<20 {
		return
	}
	// Count existing gz archives to determine next N
	pattern := filepath.Join(ms.dir, name+".read.jsonl.*.gz")
	matches, _ := filepath.Glob(pattern)
	n := len(matches)
	gzPath := filepath.Join(ms.dir, fmt.Sprintf("%s.read.jsonl.%d.gz", name, n))
	// Compress current read file
	src, err := os.Open(path)
	if err != nil {
		return
	}
	defer src.Close()
	dst, err := os.Create(gzPath)
	if err != nil {
		return
	}
	gw := gzip.NewWriter(dst)
	io.Copy(gw, src)
	gw.Close()
	dst.Close()
	// Truncate read file
	os.Truncate(path, 0)
	logger.Info(fmt.Sprintf("Mailbox rotated: %s -> %s", path, gzPath))
}

// receiveAll reads all unread messages, moves them to read.jsonl, clears unread.jsonl,
// and returns the messages.
func (ms *mailboxStore) receiveAll(name string) []mailboxMessage {
	unreadPath := ms.unreadPath(name)
	f, err := os.Open(unreadPath)
	if err != nil {
		return nil
	}
	var messages []mailboxMessage
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4<<20), 4<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg mailboxMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		msg.Read = true
		messages = append(messages, msg)
	}
	f.Close()
	if len(messages) == 0 {
		return nil
	}
	// Move messages to read.jsonl
	for _, msg := range messages {
		ms.appendToRead(name, msg)
	}
	// Truncate unread file
	os.Truncate(unreadPath, 0)
	return messages
}

// hasUnread checks if there are any unread messages for the given agent.
func (ms *mailboxStore) hasUnread(name string) bool {
	info, err := os.Stat(ms.unreadPath(name))
	return err == nil && info.Size() > 0
}

// receive returns a channel that will receive all unread messages.
// If unread messages exist, they are sent immediately; otherwise waits for new messages.
func (ms *mailboxStore) receive(name string) chan []mailboxMessage {
	ch := make(chan []mailboxMessage, 1)
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if ms.hasUnread(name) {
		msgs := ms.receiveAll(name)
		if len(msgs) > 0 {
			ch <- msgs
			return ch
		}
	}
	ms.waiters[name] = append(ms.waiters[name], ch)
	return ch
}

// cancelReceive removes a channel from the waiters list.
func (ms *mailboxStore) cancelReceive(name string, ch chan []mailboxMessage) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	list := ms.waiters[name]
	for i, c := range list {
		if c == ch {
			ms.waiters[name] = append(list[:i], list[i+1:]...)
			break
		}
	}
}

// readLastN reads the last n lines from a JSONL file.
func readLastN(path string, n int) []mailboxMessage {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var lines []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4<<20), 4<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		lines = append(lines, line)
	}
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	var result []mailboxMessage
	for _, line := range lines {
		var msg mailboxMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		result = append(result, msg)
	}
	return result
}

// readAllLines reads all lines from a JSONL file as mailboxMessages.
func readAllLines(path string) []mailboxMessage {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var result []mailboxMessage
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4<<20), 4<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg mailboxMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		result = append(result, msg)
	}
	return result
}

// inbox returns the last `limit` messages for the given agent (read + unread), most recent first.
func (ms *mailboxStore) inbox(name string, limit int) []mailboxMessage {
	// Read messages from read.jsonl (last limit lines)
	readMsgs := readLastN(ms.readPath(name), limit)
	for i := range readMsgs {
		readMsgs[i].Read = true
	}
	// All unread messages from unread.jsonl
	unreadMsgs := readAllLines(ms.unreadPath(name))
	for i := range unreadMsgs {
		unreadMsgs[i].Read = false
	}
	// Combine: read first, then unread (chronological order)
	combined := append(readMsgs, unreadMsgs...)
	// Apply limit
	if limit > 0 && len(combined) > limit {
		combined = combined[len(combined)-limit:]
	}
	// Reverse to most-recent-first
	for i, j := 0, len(combined)-1; i < j; i, j = i+1, j-1 {
		combined[i], combined[j] = combined[j], combined[i]
	}
	return combined
}

// send stores a new message, posts to TG mailbox group, and notifies any waiting receivers.
func (ms *mailboxStore) send(bot *tele.Bot, from, to, subject, text string, fileData []byte, fileName string) string {
	b := make([]byte, 8)
	rand.Read(b)
	id := hex.EncodeToString(b)
	msg := mailboxMessage{
		ID:        id,
		From:      from,
		To:        to,
		Subject:   subject,
		Text:      text,
		FileName:  fileName,
		Timestamp: time.Now(),
		Read:      false,
	}
	// Post to TG mailbox channel if configured
	creds, err := config.LoadCredentials()
	if err == nil && bot != nil {
		if creds.MailboxChatID == 0 {
			logger.Info("Mailbox channel not configured")
		} else {
			channel := &tele.Chat{ID: creds.MailboxChatID}
			timestampStr := msg.Timestamp.Format(time.RFC3339)
			if fileData != nil {
				// Attachment path: plain-text caption with status at body end,
				// truncated to TG 1024 char limit. Full content is in the attached file.
				caption := fmt.Sprintf("📤 From: %s\n📥 To: %s\n🕐 %s\n🆔 %s\n━━━━━━━━━━\nSubject: %s\n━━━━━━━━━━\n%s\n━━━━━━━━━━\n📫 Unread",
					from, to, timestampStr, id, subject, text)
				if runes := []rune(caption); len(runes) > 1024 {
					caption = string(runes[:1024])
				}
				doc := &tele.Document{
					File:     tele.FromReader(bytes.NewReader(fileData)),
					FileName: fileName,
					Caption:  caption,
				}
				tgMsg, sendErr := bot.Send(channel, doc)
				if sendErr != nil {
					logger.Error(fmt.Sprintf("Mailbox TG send failed: %v", sendErr))
				} else if tgMsg != nil {
					msg.TGMsgIDs = []int{tgMsg.ID}
					if tgMsg.Document != nil {
						msg.FileID = tgMsg.Document.FileID
					}
				}
			} else {
				chunks := buildMailboxChunks("", from, to, timestampStr, id, subject, text, "📫 Unread")
				sentMsgs, sendErr := sendMailboxChunks(bot, channel, chunks)
				if sendErr != nil {
					logger.Error(fmt.Sprintf("Mailbox TG send failed: %v", sendErr))
				}
				if len(sentMsgs) > 0 {
					msg.TGMsgIDs = make([]int, len(sentMsgs))
					for i, m := range sentMsgs {
						msg.TGMsgIDs[i] = m.ID
					}
				}
				logger.Info(fmt.Sprintf("Mailbox channel post: id=%s chunks=%d sent=%d tg_msg_ids=%v", id, len(chunks), len(sentMsgs), msg.TGMsgIDs))
			}
		}
	}
	ms.mu.Lock()
	// Write to unread file
	ms.appendToUnread(to, msg)
	// Notify waiters with a slice containing the new message
	waiters := ms.waiters[to]
	if len(waiters) > 0 {
		newMsg := []mailboxMessage{msg}
		for _, ch := range waiters {
			select {
			case ch <- newMsg:
			default:
			}
		}
		ms.waiters[to] = nil
	}
	ms.mu.Unlock()
	return id
}

// editTGReadReceipt edits the LAST TG message of a mailbox notification to mark it as read.
// status is appended to the end of the last chunk (at body bottom), so multi-chunk messages
// only need the final message rewritten; earlier chunks contain no status and stay unchanged.
func editTGReadReceipt(bot *tele.Bot, msg mailboxMessage, chatID int64) {
	if len(msg.TGMsgIDs) == 0 {
		return
	}
	readTime := time.Now().Format("2006-01-02 15:04")
	status := "📭 Read — " + readTime
	lastMsgID := msg.TGMsgIDs[len(msg.TGMsgIDs)-1]
	editMsg := &tele.Message{ID: lastMsgID, Chat: &tele.Chat{ID: chatID}}
	if msg.FileName != "" {
		// Attachment path: plain-text caption with status at body end, truncated to 1024.
		caption := fmt.Sprintf("📤 From: %s\n📥 To: %s\n🕐 %s\n🆔 %s\n━━━━━━━━━━\nSubject: %s\n━━━━━━━━━━\n%s\n━━━━━━━━━━\n%s",
			msg.From, msg.To, msg.Timestamp.Format(time.RFC3339), msg.ID, msg.Subject, msg.Text, status)
		if runes := []rune(caption); len(runes) > 1024 {
			caption = string(runes[:1024])
		}
		bot.EditCaption(editMsg, caption)
		return
	}
	chunks := buildMailboxChunks("", msg.From, msg.To, msg.Timestamp.Format(time.RFC3339), msg.ID, msg.Subject, msg.Text, status)
	if _, err := helpers.RetryEdit(bot, editMsg, chunks[len(chunks)-1], tele.ModeHTML); err != nil {
		logger.Error(fmt.Sprintf("editTGReadReceipt failed: msg_id=%d err=%v", lastMsgID, err))
	}
}

// registerMailboxAPI registers mailbox HTTP endpoints on the given mux.
func registerMailboxAPI(mux *http.ServeMux, bs *BotState) {
	bot := bs.Bot
	// POST /mailbox/send — send a message from one agent to another (multipart form)
	mux.HandleFunc("/mailbox/send", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		// Parse multipart form (max 20MB — TG Bot API download limit)
		r.ParseMultipartForm(20 << 20)
		from := r.FormValue("from")
		to := r.FormValue("to")
		subject := r.FormValue("subject")
		text := r.FormValue("text")
		if from == "" || to == "" || text == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "from, to, text are required"})
			return
		}
		var fileData []byte
		var fileName string
		file, header, err := r.FormFile("file")
		if err == nil {
			defer file.Close()
			// Check file size (20MB limit — TG Bot API download limit)
			if header.Size > 20<<20 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]any{
					"ok":    false,
					"error": fmt.Sprintf("file too large: %d bytes (max 20MB)", header.Size),
				})
				return
			}
			fileData, _ = io.ReadAll(file)
			fileName = header.Filename
		}
		msgID := mailbox.send(bot, from, to, subject, text, fileData, fileName)
		logger.Info(fmt.Sprintf("Mailbox send: from=%s to=%s subject=%s text=%s", from, to, subject, text))
		// Send TG notification to target agent's chat
		targetInfo := bs.SessionState.FindByName(to)
		if targetInfo != nil {
			chat, _, topicID := resolveChat(bs, targetInfo.TmuxTarget)
			if chat != nil {
				timestampStr := time.Now().Format(time.RFC3339)
				chunks := buildMailboxChunks("📥 Mail Received", from, to, timestampStr, msgID, subject, text, "")
				var sendOpts []interface{}
				if topicID > 0 {
					sendOpts = append(sendOpts, &tele.SendOptions{ThreadID: topicID})
				}
				if _, err := sendMailboxChunks(bot, chat, chunks, sendOpts...); err != nil {
					logger.Error(fmt.Sprintf("Mailbox receiver notify failed: to=%s id=%s err=%v", to, msgID, err))
				}
				logger.Info(fmt.Sprintf("Mailbox receiver notify: to=%s id=%s chunks=%d", to, msgID, len(chunks)))
			}
		}
		// Notify sender's chat
		fromInfo := bs.SessionState.FindByName(from)
		if fromInfo != nil {
			fromChat, _, fromTopicID := resolveChat(bs, fromInfo.TmuxTarget)
			if fromChat != nil {
				timestampStr := time.Now().Format(time.RFC3339)
				chunks := buildMailboxChunks("📤 Mail Sent", from, to, timestampStr, msgID, subject, text, "")
				var senderOpts []interface{}
				if fromTopicID > 0 {
					senderOpts = append(senderOpts, &tele.SendOptions{ThreadID: fromTopicID})
				}
				if _, err := sendMailboxChunks(bot, fromChat, chunks, senderOpts...); err != nil {
					logger.Error(fmt.Sprintf("Mailbox sender notify failed: from=%s id=%s err=%v", from, msgID, err))
				}
				logger.Info(fmt.Sprintf("Mailbox sender notify: from=%s id=%s chunks=%d", from, msgID, len(chunks)))
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "id": msgID})
	})

	// GET /mailbox/receive?name=<name> — long-poll for all unread messages
	mux.HandleFunc("/mailbox/receive", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		if name == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "name required"})
			return
		}
		ch := mailbox.receive(name)
		select {
		case msgs := <-ch:
			// Edit TG messages with read receipts
			mailboxCreds, _ := config.LoadCredentials()
			if mailboxCreds.MailboxChatID != 0 {
				for _, msg := range msgs {
					editTGReadReceipt(bot, msg, mailboxCreds.MailboxChatID)
				}
			}
			// Move messages from unread to read
			mailbox.receiveAll(name)
			logger.Info(fmt.Sprintf("Mailbox receive: name=%s count=%d", name, len(msgs)))
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"messages": msgs})
		case <-r.Context().Done():
			mailbox.cancelReceive(name, ch)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "timeout"})
		}
	})

	// GET /mailbox/inbox?name=<name>&limit=<N> — list recent messages for an agent
	mux.HandleFunc("/mailbox/inbox", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		if name == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "name required"})
			return
		}
		limit := 20
		if lStr := r.URL.Query().Get("limit"); lStr != "" {
			if n, err := strconv.Atoi(lStr); err == nil && n > 0 {
				limit = n
			}
		}
		messages := mailbox.inbox(name, limit)
		if messages == nil {
			messages = []mailboxMessage{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"messages": messages})
	})

	// GET /mailbox/download?id=<msg_id> — download attachment by message ID
	mux.HandleFunc("/mailbox/download", func(w http.ResponseWriter, r *http.Request) {
		msgID := r.URL.Query().Get("id")
		if msgID == "" {
			http.Error(w, "id required", 400)
			return
		}
		// Search all JSONL files for the message
		var found *mailboxMessage
		entries, _ := os.ReadDir(mailbox.dir)
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			f, err := os.Open(filepath.Join(mailbox.dir, e.Name()))
			if err != nil {
				continue
			}
			scanner := bufio.NewScanner(f)
			scanner.Buffer(make([]byte, 4<<20), 4<<20)
			for scanner.Scan() {
				var msg mailboxMessage
				if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
					continue
				}
				if msg.ID == msgID {
					found = &msg
					break
				}
			}
			f.Close()
			if found != nil {
				break
			}
		}
		if found == nil {
			http.Error(w, "message not found", 404)
			return
		}
		if found.FileID == "" {
			http.Error(w, "no attachment", 404)
			return
		}
		// Download from TG by FileID
		tgFile, err := bot.FileByID(found.FileID)
		if err != nil {
			http.Error(w, fmt.Sprintf("TG file error: %v", err), 500)
			return
		}
		reader, err := bot.File(&tgFile)
		if err != nil {
			http.Error(w, fmt.Sprintf("TG download error: %v", err), 500)
			return
		}
		defer reader.Close()
		// Stream file to response with original filename
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", found.FileName))
		io.Copy(w, reader)
	})
}
