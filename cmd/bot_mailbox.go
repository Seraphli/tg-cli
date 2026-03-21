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

	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/logger"
	tele "gopkg.in/telebot.v3"
)

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
	TGMsgID   int       `json:"tg_msg_id,omitempty"`
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
// and returns (messages, tgMsgIDs).
func (ms *mailboxStore) receiveAll(name string) ([]mailboxMessage, []int) {
	unreadPath := ms.unreadPath(name)
	f, err := os.Open(unreadPath)
	if err != nil {
		return nil, nil
	}
	var messages []mailboxMessage
	var tgMsgIDs []int
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
		if msg.TGMsgID != 0 {
			tgMsgIDs = append(tgMsgIDs, msg.TGMsgID)
		}
	}
	f.Close()
	if len(messages) == 0 {
		return nil, nil
	}
	// Move messages to read.jsonl
	for _, msg := range messages {
		ms.appendToRead(name, msg)
	}
	// Truncate unread file
	os.Truncate(unreadPath, 0)
	return messages, tgMsgIDs
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
		msgs, _ := ms.receiveAll(name)
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
	// Build formatted TG message
	formatted := fmt.Sprintf("📤 From: %s\n📥 To: %s\n🕐 %s\n🆔 %s\n━━━━━━━━━━\nSubject: %s\n━━━━━━━━━━\n%s\n━━━━━━━━━━\n📫 Unread",
		from, to, msg.Timestamp.Format(time.RFC3339), id, subject, text)
	// Post to TG mailbox channel if configured
	creds, err := config.LoadCredentials()
	if err == nil && bot != nil {
		if creds.MailboxChatID == 0 {
			logger.Info("Mailbox channel not configured")
		} else {
			channel := &tele.Chat{ID: creds.MailboxChatID}
			var tgMsg *tele.Message
			if fileData != nil {
				caption := formatted
				if len(caption) > 1024 {
					caption = caption[:1024]
				}
				doc := &tele.Document{
					File:     tele.FromReader(bytes.NewReader(fileData)),
					FileName: fileName,
					Caption:  caption,
				}
				tgMsg, err = bot.Send(channel, doc)
			} else {
				tgMsg, err = retrySend(bot, channel, formatted)
			}
			if err != nil {
				logger.Error(fmt.Sprintf("Mailbox TG send failed: %v", err))
			} else if tgMsg != nil {
				msg.TGMsgID = tgMsg.ID
				if tgMsg.Document != nil {
					msg.FileID = tgMsg.Document.FileID
				}
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

// editTGReadReceipt edits a TG message to mark it as read.
func editTGReadReceipt(bot *tele.Bot, msg mailboxMessage, chatID int64) {
	if msg.TGMsgID == 0 {
		return
	}
	readTime := time.Now().Format("2006-01-02 15:04")
	newText := fmt.Sprintf("📤 From: %s\n📥 To: %s\n🕐 %s\n🆔 %s\n━━━━━━━━━━\nSubject: %s\n━━━━━━━━━━\n%s\n━━━━━━━━━━\n📭 Read — %s",
		msg.From, msg.To, msg.Timestamp.Format(time.RFC3339), msg.ID, msg.Subject, msg.Text, readTime)
	editMsg := &tele.Message{ID: msg.TGMsgID, Chat: &tele.Chat{ID: chatID}}
	if msg.FileName != "" {
		bot.Edit(editMsg, newText, &tele.SendOptions{})
	} else {
		bot.Edit(editMsg, newText)
	}
}

// registerMailboxAPI registers mailbox HTTP endpoints on the given mux.
func registerMailboxAPI(mux *http.ServeMux, bot *tele.Bot, creds *config.Credentials) {
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
		maxBody := 3500
		totalLen := len(subject) + len(text)
		if totalLen > maxBody {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]any{
				"ok":    false,
				"error": fmt.Sprintf("content too long: subject+text=%d chars (max %d). Use --file for long content.", totalLen, maxBody),
			})
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
		logger.Info(fmt.Sprintf("Mailbox send: from=%s to=%s subject=%s text=%s", from, to, subject, truncateStr(text, 200)))
		// Send TG notification to target agent's chat
		targetInfo := sessionState.findByName(to)
		if targetInfo != nil {
			chat, _, topicID := resolveChat(targetInfo.tmuxTarget)
			if chat != nil {
				receiverNotify := fmt.Sprintf("📥 Mail Received\n📤 From: %s\n🕐 %s\n🆔 %s\n━━━━━━━━━━\nSubject: %s\n━━━━━━━━━━\n%s",
					from, time.Now().Format(time.RFC3339), msgID, subject, text)
				var sendOpts []interface{}
				if topicID > 0 {
					sendOpts = append(sendOpts, &tele.SendOptions{ThreadID: topicID})
				}
				chunks := splitBody(receiverNotify, 4000)
				if len(chunks) <= 1 {
					retrySend(bot, chat, receiverNotify, sendOpts...)
				} else {
					retrySend(bot, chat, chunks[0]+fmt.Sprintf("\n\n📄 1/%d", len(chunks)), sendOpts...)
				}
			}
		}
		// Notify sender's chat
		fromInfo := sessionState.findByName(from)
		if fromInfo != nil {
			fromChat, _, fromTopicID := resolveChat(fromInfo.tmuxTarget)
			if fromChat != nil {
				senderNotify := fmt.Sprintf("📤 Mail Sent\n📥 To: %s\n🕐 %s\n🆔 %s\n━━━━━━━━━━\nSubject: %s\n━━━━━━━━━━\n%s",
					to, time.Now().Format(time.RFC3339), msgID, subject, text)
				var senderOpts []interface{}
				if fromTopicID > 0 {
					senderOpts = append(senderOpts, &tele.SendOptions{ThreadID: fromTopicID})
				}
				chunks := splitBody(senderNotify, 4000)
				if len(chunks) <= 1 {
					retrySend(bot, fromChat, senderNotify, senderOpts...)
				} else {
					retrySend(bot, fromChat, chunks[0]+fmt.Sprintf("\n\n📄 1/%d", len(chunks)), senderOpts...)
				}
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
