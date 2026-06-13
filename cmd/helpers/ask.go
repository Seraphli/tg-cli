package helpers

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/logger"
	tele "gopkg.in/telebot.v3"
)

// AskStateAccessor provides access to bot state for ask operations.
// Using individual fields avoids circular imports with cmd package.
type AskStateAccessor struct {
	Bot          *tele.Bot
	ToolNotifs   *stores.ToolNotifyStore
	PendingFiles *stores.PendingFileStore
}

// DoRespondAsk responds to AskUserQuestion: push answer to wait store + edit TG msg.
func DoRespondAsk(
	bot *tele.Bot,
	toolNotifs *stores.ToolNotifyStore,
	pendingFiles *stores.PendingFileStore,
	pendingWait *stores.PendingWaitStore,
	reactionTracker *stores.ReactionTrackerStore,
	msgID int,
	answers map[string]string,
	frozenLabel string,
) error {
	uuid, ok := pendingFiles.Get(msgID)
	if !ok {
		// Fallback: look up entry from wait store by msg_id
		if waitEntry, wok := pendingWait.FindByMsgID(msgID); wok {
			uuid = waitEntry.UUID
		} else {
			return fmt.Errorf("pending entry not found")
		}
	}
	waitEntry, wok := pendingWait.Get(uuid)
	if !wok {
		cleanupAskState(bot, toolNotifs, pendingFiles, msgID, uuid, "wait entry missing")
		return fmt.Errorf("hook dead (stale pending)")
	}
	ccOutput := BuildAskCCOutput(waitEntry.Payload, answers)
	WritePendingAnswer(pendingWait, uuid, ccOutput)
	if entry, entryOk := toolNotifs.Get(msgID); entryOk {
		toolNotifs.MarkResolved(msgID)
		editMsg := &tele.Message{ID: msgID, Chat: &tele.Chat{ID: entry.ChatID}}
		RetryEdit(bot, editMsg, entry.MsgText, BuildFrozenMarkup(entry, frozenLabel), tele.ModeHTML)
		reactionTracker.RecordPending(entry.TmuxTarget, entry.ChatID, msgID)
	}
	logger.Info(fmt.Sprintf("AskUserQuestion responded: msg_id=%d uuid=%s answers=%v", msgID, uuid, answers))
	return nil
}

// DoCancelAsk cancels an AskUserQuestion: push cancel to wait store + ESC + edit TG msg.
func DoCancelAsk(
	bot *tele.Bot,
	toolNotifs *stores.ToolNotifyStore,
	pendingFiles *stores.PendingFileStore,
	pendingWait *stores.PendingWaitStore,
	extractTarget func(string) (*injector.TmuxTarget, error),
	msgID int,
) string {
	uuid, _ := pendingFiles.Get(msgID)
	if uuid == "" {
		if waitEntry, wok := pendingWait.FindByMsgID(msgID); wok {
			uuid = waitEntry.UUID
		}
	}
	if uuid != "" {
		// Push cancel to wait store (no Remove — live handler removes on delivery)
		pendingWait.Push(uuid, stores.WaitEvent{Type: "cancel"})
	}
	if entry, ok := toolNotifs.Get(msgID); ok {
		targetPtr, err := extractTarget(entry.MsgText)
		if err == nil && targetPtr != nil {
			injector.SendKeys(*targetPtr, "Escape")
		}
		toolNotifs.MarkResolved(msgID)
		editMsg := &tele.Message{ID: msgID, Chat: &tele.Chat{ID: entry.ChatID}}
		RetryEdit(bot, editMsg, entry.MsgText, BuildFrozenMarkup(entry, "❌ Cancelled"), tele.ModeHTML)
	}
	logger.Info(fmt.Sprintf("AskUserQuestion cancelled: msg_id=%d uuid=%s", msgID, uuid))
	return uuid
}

// DoChatAsk handles chat mode for AskUserQuestion: push __chat answer to wait store + edit.
func DoChatAsk(
	bot *tele.Bot,
	toolNotifs *stores.ToolNotifyStore,
	pendingFiles *stores.PendingFileStore,
	pendingWait *stores.PendingWaitStore,
	reactionTracker *stores.ReactionTrackerStore,
	msgID int,
) error {
	uuid, ok := pendingFiles.Get(msgID)
	if !ok {
		if waitEntry, wok := pendingWait.FindByMsgID(msgID); wok {
			uuid = waitEntry.UUID
		} else {
			return fmt.Errorf("pending entry not found")
		}
	}
	waitEntry, wok := pendingWait.Get(uuid)
	if !wok {
		cleanupAskState(bot, toolNotifs, pendingFiles, msgID, uuid, "wait entry missing on chat button")
		return fmt.Errorf("question expired")
	}
	answers := map[string]string{"__chat": "true"}
	ccOutput := BuildAskCCOutput(waitEntry.Payload, answers)
	WritePendingAnswer(pendingWait, uuid, ccOutput)
	if entry, entryOk := toolNotifs.Get(msgID); entryOk {
		toolNotifs.MarkResolved(msgID)
		editMsg := &tele.Message{ID: msgID, Chat: &tele.Chat{ID: entry.ChatID}}
		RetryEdit(bot, editMsg, entry.MsgText, BuildFrozenMarkup(entry, "💬 Chat mode selected"), tele.ModeHTML)
		reactionTracker.RecordPending(entry.TmuxTarget, entry.ChatID, msgID)
	}
	logger.Info(fmt.Sprintf("AskUserQuestion chat mode: msg_id=%d uuid=%s", msgID, uuid))
	return nil
}

// CheckSessionAlive checks if a tmux session still exists; calls cleanDead if dead.
func CheckSessionAlive(tmuxTarget string, cleanDead func(string)) bool {
	target, err := injector.ParseTarget(tmuxTarget)
	if err != nil {
		return false
	}
	if injector.SessionExists(target) {
		return true
	}
	cleanDead(tmuxTarget)
	return false
}

// SafeInjectTextParams holds all parameters for SafeInjectText to avoid a large arg list.
type SafeInjectTextParams struct {
	Bot              *tele.Bot
	ToolNotifs       *stores.ToolNotifyStore
	PendingFiles     *stores.PendingFileStore
	PendingWait      *stores.PendingWaitStore
	PendingPerms     *stores.PendingPermStore
	InjectQueue      *stores.InjectQueueStore
	InjectConfirm    *stores.InjectConfirmStore
	StopCooldown     *stores.StopCooldownStore
	ReactionTracker  *stores.ReactionTrackerStore
	SessionState     *stores.SessionStateStore
	HookSessionLocks *sync.Map
	SessionEvents    *stores.SessionEventStore
	ResolveChat      func(string) (*tele.Chat, string, int)
	FormatPaneID     func(string) string
	Force            bool   // Skip busy check — used by flushInjectQueue
	AltSnippet       string // Alternative snippet for CapturePane (e.g. "[Image" for image inject)
}

// injectResult carries the outcome of safeInjectPhase1 to safeInjectPhase2.
type injectResult struct {
	err           error
	ch            chan bool
	confirmType   string // "askq", "prompt", ""
	shouldSubmit  bool
	captureTarget injector.TmuxTarget
	snippet       string
	altSnippet    string
}

// SafeInjectText checks for pending AskUserQuestion/PermissionRequest on the target pane.
// If AskUserQuestion is pending, answers it with the text and returns. Otherwise injects text directly.
// Phase1 runs inside Dispatch (state check + inject); phase2 runs outside (CapturePane + confirmation wait).
func SafeInjectText(p SafeInjectTextParams, tmuxTarget string, text string, submit ...bool) error {
	if p.SessionEvents != nil {
		sid, _ := p.SessionState.FindByTarget(tmuxTarget)
		if sid != "" {
			saved := p.SessionEvents
			p.SessionEvents = nil
			var res injectResult
			dispatchErr := saved.Dispatch(sid, "inject:safe", func() error {
				res = safeInjectPhase1(p, tmuxTarget, text, submit...)
				return nil
			})
			if dispatchErr != nil {
				return dispatchErr
			}
			if res.err != nil {
				return res.err
			}
			if res.confirmType != "" {
				return safeInjectPhase2(p, tmuxTarget, res)
			}
			return nil
		}
	}
	res := safeInjectPhase1(p, tmuxTarget, text, submit...)
	if res.err != nil {
		return res.err
	}
	if res.confirmType != "" {
		return safeInjectPhase2(p, tmuxTarget, res)
	}
	return nil
}

// truncateQueueTexts joins queued texts and truncates to maxRunes if needed.
// Appends a truncation marker showing item count when truncated.
func truncateQueueTexts(texts []string, maxRunes int) string {
	joined := strings.Join(texts, "\n")
	r := []rune(joined)
	if len(r) <= maxRunes {
		return joined
	}
	return string(r[:maxRunes]) + fmt.Sprintf("\n… (%d items, truncated)", len(texts))
}

// safeInjectPhase1 handles state check + inject/answer/queue.
// Returns injectResult; confirmation wait and CapturePane are deferred to safeInjectPhase2.
func safeInjectPhase1(p SafeInjectTextParams, tmuxTarget string, text string, submit ...bool) injectResult {
	// Acquire per-session lock to serialize with hook processing
	// Lock covers state check + injection only; released before CapturePane and confirmation wait
	var sessionMu *sync.Mutex
	if p.HookSessionLocks != nil && p.SessionState != nil {
		if sid, found := p.SessionState.FindByTarget(tmuxTarget); found && sid != "" {
			v, _ := p.HookSessionLocks.LoadOrStore(sid, &sync.Mutex{})
			sessionMu = v.(*sync.Mutex)
			sessionMu.Lock()
		}
	}
	target, err := injector.ParseTarget(tmuxTarget)
	if err != nil {
		return injectResult{err: err}
	}
	// PRE-INJECT: check if there's a pending AskUserQuestion
	_, _, hasAskQ := p.ToolNotifs.FindByTmuxTarget(tmuxTarget)
	if !p.Force && IsSessionRunning(tmuxTarget) && !hasAskQ {
		chat, chatIDStr, topicID := p.ResolveChat(tmuxTarget)
		chatIDInt := int64(0)
		for _, c := range chatIDStr {
			if c >= '0' && c <= '9' {
				chatIDInt = chatIDInt*10 + int64(c-'0')
			}
		}
		p.InjectQueue.Enqueue(tmuxTarget, stores.InjectItem{Text: text, ChatID: chatIDInt, TopicID: topicID})
		count := p.InjectQueue.ItemCount(tmuxTarget)
		logger.Info(fmt.Sprintf("safeInjectText: CC busy, queued for target=%s count=%d text=%s", tmuxTarget, count, strings.ReplaceAll(text, "\n", "\\n")))
		if chat != nil {
			allTexts := p.InjectQueue.GetTexts(tmuxTarget)
			queueID := p.InjectQueue.GetInjectID(tmuxTarget)
			notifyText := fmt.Sprintf("⏳ Queued [%s] (%d)\n📟 %s\n──────\n%s\n──────", queueID, count, p.FormatPaneID(tmuxTarget), truncateQueueTexts(allTexts, 3500))
			var sendOpts []interface{}
			if topicID > 0 {
				sendOpts = append(sendOpts, &tele.SendOptions{ThreadID: topicID})
			}
			if existingMsgID, ok := p.InjectQueue.GetNotifyMsg(tmuxTarget); ok {
				editMsg := &tele.Message{ID: existingMsgID, Chat: chat}
				RetryEdit(p.Bot, editMsg, notifyText)
			} else {
				sent, _ := RetrySend(p.Bot, chat, notifyText, sendOpts...)
				if sent != nil {
					p.InjectQueue.SetNotifyMsg(tmuxTarget, sent.ID)
				}
			}
		}
		if sessionMu != nil { sessionMu.Unlock() }
		return injectResult{}
	}
	// Answer pending AskUserQuestion with the text
	for {
		msgID, entry, ok := p.ToolNotifs.FindByTmuxTarget(tmuxTarget)
		if !ok {
			break
		}
		uuid, uuidOk := p.PendingFiles.Get(msgID)
		if !uuidOk {
			p.ToolNotifs.MarkResolved(msgID)
			continue
		}
		// Check wait store liveness instead of file-based stale check
		waitEntry, waitOk := p.PendingWait.Get(uuid)
		if !waitOk {
			cleanupAskState(p.Bot, p.ToolNotifs, p.PendingFiles, msgID, uuid, "wait entry missing")
			continue
		}
		answers := make(map[string]string)
		if len(entry.Questions) > 0 {
			answers[entry.Questions[0].QuestionText] = text
		}
		ccOutput := BuildAskCCOutput(waitEntry.Payload, answers)
		WritePendingAnswer(p.PendingWait, uuid, ccOutput)
		p.ToolNotifs.MarkResolved(msgID)
		if sessionMu != nil { sessionMu.Unlock() }
		editMsg := &tele.Message{ID: msgID, Chat: &tele.Chat{ID: entry.ChatID}}
		RetryEdit(p.Bot, editMsg, entry.MsgText, BuildFrozenMarkup(entry, "✅ Custom reply"), tele.ModeHTML)
		logger.Info(fmt.Sprintf("safeInjectText: answered AskUserQuestion msg_id=%d uuid=%s text=%s", msgID, uuid, TruncateStr(text, 200)))
		// Register confirmation channel — wait happens in phase2
		ch := p.InjectConfirm.Register(tmuxTarget, stores.ConfirmAskAnswered, text)
		return injectResult{ch: ch, confirmType: "askq"}
	}
	// PermissionRequest pending — queue instead of injecting
	if _, ok := p.PendingPerms.FindByTmuxTarget(tmuxTarget); ok {
		chat, chatIDStr, topicID := p.ResolveChat(tmuxTarget)
		chatIDInt := int64(0)
		for _, c := range chatIDStr {
			if c >= '0' && c <= '9' {
				chatIDInt = chatIDInt*10 + int64(c-'0')
			}
		}
		p.InjectQueue.Enqueue(tmuxTarget, stores.InjectItem{Text: text, ChatID: chatIDInt, TopicID: topicID})
		count := p.InjectQueue.ItemCount(tmuxTarget)
		logger.Info(fmt.Sprintf("safeInjectText: PermissionRequest pending, queued for target=%s count=%d text=%s", tmuxTarget, count, TruncateStr(text, 200)))
		if chat != nil {
			allTexts := p.InjectQueue.GetTexts(tmuxTarget)
			queueID := p.InjectQueue.GetInjectID(tmuxTarget)
			notifyText := fmt.Sprintf("⏳ Queued [%s] (%d)\n📟 %s\n🔒 PermissionRequest pending\n──────\n%s\n──────", queueID, count, p.FormatPaneID(tmuxTarget), truncateQueueTexts(allTexts, 3500))
			var sendOpts []interface{}
			if topicID > 0 {
				sendOpts = append(sendOpts, &tele.SendOptions{ThreadID: topicID})
			}
			if existingMsgID, ok := p.InjectQueue.GetNotifyMsg(tmuxTarget); ok {
				editMsg := &tele.Message{ID: existingMsgID, Chat: chat}
				RetryEdit(p.Bot, editMsg, notifyText)
			} else {
				sent, _ := RetrySend(p.Bot, chat, notifyText, sendOpts...)
				if sent != nil {
					p.InjectQueue.SetNotifyMsg(tmuxTarget, sent.ID)
				}
			}
		}
		if sessionMu != nil { sessionMu.Unlock() }
		return injectResult{}
	}
	logger.Info(fmt.Sprintf("safeInjectText: direct inject path, target=%s text=%s", tmuxTarget, TruncateStr(text, 200)))
	// Wait for Stop event cooldown before injecting
	p.StopCooldown.WaitIfNeeded(tmuxTarget, 3*time.Second)
	shouldSubmit := len(submit) == 0 || submit[0]
	ch := p.InjectConfirm.Register(tmuxTarget, stores.ConfirmUserPromptSubmit, text)
	if err := injector.InjectText(target, text, shouldSubmit); err != nil {
		p.InjectConfirm.Cancel(tmuxTarget)
		if sessionMu != nil { sessionMu.Unlock() }
		return injectResult{err: err}
	}
	// Release lock after injection — CapturePane and confirmation wait happen in phase2
	if sessionMu != nil { sessionMu.Unlock() }
	snippet := text
	if idx := strings.Index(snippet, "\n"); idx >= 0 {
		snippet = snippet[:idx]
	}
	if len(snippet) > 50 {
		snippet = snippet[:50]
	}
	return injectResult{ch: ch, confirmType: "prompt", captureTarget: target, snippet: snippet, altSnippet: p.AltSnippet, shouldSubmit: shouldSubmit}
}

// safeInjectPhase2 handles CapturePane verification and confirmation wait.
// Runs OUTSIDE Dispatch so hook handlers can deliver signals into the queue.
func safeInjectPhase2(p SafeInjectTextParams, tmuxTarget string, res injectResult) error {
	if res.confirmType == "askq" {
		select {
		case ok := <-res.ch:
			if ok {
				p.ReactionTracker.PromotePending(p.Bot, tmuxTarget)
				logger.Info(fmt.Sprintf("safeInjectText: AskQ answer confirmed via PostToolUse, target=%s", tmuxTarget))
			} else {
				logger.Info(fmt.Sprintf("safeInjectText: AskQ answer content mismatch, target=%s", tmuxTarget))
			}
		case <-time.After(30 * time.Second):
			p.InjectConfirm.Cancel(tmuxTarget)
			logger.Info(fmt.Sprintf("safeInjectText: AskQ answer not confirmed (PostToolUse timeout), target=%s", tmuxTarget))
		}
		return nil
	}
	// confirmType == "prompt"
	// CapturePane verification — scan bottom-up, distinguish idle/staged/submitted states
	promptChars := []string{"❯"}
	if p.SessionState != nil {
		if info := p.SessionState.FindInfoByTarget(tmuxTarget); info != nil {
			switch info.Backend {
			case "codex":
				promptChars = []string{"›"}
			case "cc":
				promptChars = []string{"❯"}
			default:
				promptChars = []string{"❯", "›"}
			}
		}
	}
	captureConfirmed := false
	var lastCaptureContent string
	var captureState string
	for attempt := 0; attempt < 3; attempt++ {
		time.Sleep(500 * time.Millisecond)
		captureContent, captureErr := injector.CapturePane(res.captureTarget)
		if captureErr != nil {
			continue
		}
		lastCaptureContent = captureContent
		lines := strings.Split(captureContent, "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			line := lines[i]
			raw := strings.TrimRight(line, " \t")
			idx := -1
			pcLen := 0
			for _, pc := range promptChars {
				if j := strings.Index(raw, pc); j >= 0 {
					idx = j
					pcLen = len(pc)
					break
				}
			}
			if idx < 0 {
				continue
			}
			after := raw[idx+pcLen:]
			after = strings.TrimLeft(after, " \xc2\xa0")
			matched := strings.Contains(after, res.snippet)
			if !matched && res.altSnippet != "" {
				matched = strings.Contains(after, res.altSnippet)
			}
			if after == "" || !matched {
				continue
			}
			leading := raw[:idx]
			if leading == "" {
				captureState = "input"
			} else if strings.TrimSpace(leading) == "" {
				captureState = "staged"
			} else {
				captureState = "submitted"
			}
			captureConfirmed = true
			break
		}
		if captureConfirmed {
			break
		}
	}
	if !captureConfirmed && lastCaptureContent != "" {
		all := strings.Split(lastCaptureContent, "\n")
		start := len(all) - 15
		if start < 0 {
			start = 0
		}
		logger.Debug(fmt.Sprintf("safeInjectText: capturePane MISS snippet=%q altSnippet=%q promptChars=%v pane_tail:\n%s",
			res.snippet, res.altSnippet, promptChars, strings.Join(all[start:], "\n")))
	}
	logger.Debug(fmt.Sprintf("safeInjectText: capturePane=%v state=%s target=%s", captureConfirmed, captureState, tmuxTarget))
	confirmed := captureConfirmed
	if !confirmed && res.shouldSubmit {
		select {
		case ok := <-res.ch:
			if ok {
				confirmed = true
			} else {
				logger.Info(fmt.Sprintf("safeInjectText: UserPromptSubmit content mismatch, target=%s", tmuxTarget))
			}
		case <-time.After(10 * time.Second):
			p.InjectConfirm.Cancel(tmuxTarget)
			logger.Debug(fmt.Sprintf("safeInjectText: inject confirmation timeout for target=%s", tmuxTarget))
		}
	}
	if confirmed {
		p.ReactionTracker.PromotePending(p.Bot, tmuxTarget)
		logger.Info(fmt.Sprintf("safeInjectText: inject confirmed, target=%s capturePane=%v", tmuxTarget, captureConfirmed))
	} else {
		logger.Info(fmt.Sprintf("safeInjectText: inject not confirmed, target=%s", tmuxTarget))
		return fmt.Errorf("inject not confirmed for target=%s", tmuxTarget)
	}
	if !res.shouldSubmit {
		p.InjectConfirm.Cancel(tmuxTarget)
	}
	return nil
}

// RebuildInMemoryState reconstructs in-memory button state for a reconnecting hook.
// Accepts explicit fields (msgID, chatID, topicID, payload, toolName, uuid, tmuxTarget) so it
// can be used by both ScanPendingDir (which reads from a PendingFile) and the new connect handler
// (which has the payload from the HTTP body). msg_text is rebuilt from payload contents.
func RebuildInMemoryState(
	toolNotifs *stores.ToolNotifyStore,
	pendingFiles *stores.PendingFileStore,
	pendingPerms *stores.PendingPermStore,
	msgID int,
	chatID int64,
	topicID int,
	payload json.RawMessage,
	toolName string,
	uuid string,
	tmuxTarget string,
	formatPaneID func(string) string,
) error {
	var p hookPayloadForRebuild
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("unmarshal payload: %w", err)
	}
	fmtTarget := formatPaneID(tmuxTarget)
	if toolName == "AskUserQuestion" {
		var askInput struct {
			Questions []struct {
				Header   string `json:"header"`
				Question string `json:"question"`
				Options  []struct {
					Label       string `json:"label"`
					Description string `json:"description"`
				} `json:"options"`
				MultiSelect bool `json:"multiSelect"`
			} `json:"questions"`
		}
		if err := json.Unmarshal(p.ToolInput, &askInput); err != nil {
			return fmt.Errorf("unmarshal tool_input: %w", err)
		}
		if len(askInput.Questions) == 0 {
			return fmt.Errorf("no questions in payload")
		}
		var qMetas []stores.QuestionMeta
		for _, q := range askInput.Questions {
			var labels []string
			for _, o := range q.Options {
				labels = append(labels, o.Label)
			}
			qMetas = append(qMetas, stores.QuestionMeta{
				QuestionText: q.Question, Header: q.Header,
				NumOptions: len(q.Options), OptionLabels: labels,
				MultiSelect: q.MultiSelect, SelectedOptions: make(map[int]bool),
				SelectedOption: -1,
			})
		}
		var qSummaries []string
		for _, q := range askInput.Questions {
			var labels []string
			for _, o := range q.Options {
				labels = append(labels, o.Label)
			}
			qSummaries = append(qSummaries, fmt.Sprintf("%s:[%s]", q.Header, strings.Join(labels, ",")))
		}
		contentSummary := strings.Join(qSummaries, " | ")
		// Rebuild msg_text from payload (no file read needed)
		var msgText string
		if p.MsgText != "" {
			msgText = p.MsgText
		}
		toolNotifs.Store(msgID, &stores.ToolNotifyEntry{
			TmuxTarget: fmtTarget, ToolName: "AskUserQuestion",
			Questions: qMetas, ChatID: chatID, MsgText: msgText,
			PendingUUID: uuid,
		})
		pendingFiles.Store(msgID, uuid)
		logger.Info(fmt.Sprintf("RebuildInMemoryState: AskUserQuestion msg_id=%d questions=%d tmux=%s content=%s uuid=%s", msgID, len(askInput.Questions), fmtTarget, contentSummary, uuid))
		return nil
	}
	// PermissionRequest: rebuild pendingPerms
	var suggestions []json.RawMessage
	json.Unmarshal(p.PermSuggestions, &suggestions)
	suggestionsRaw, _ := json.Marshal(suggestions)
	// Rebuild msg_text from payload if stored
	msgText := p.MsgText
	pendingPerms.Create(msgID, fmtTarget, suggestionsRaw, msgText, chatID, uuid)
	pendingFiles.Store(msgID, uuid)
	logger.Info(fmt.Sprintf("RebuildInMemoryState: PermissionRequest msg_id=%d tool=%s tmux=%s uuid=%s", msgID, toolName, fmtTarget, uuid))
	return nil
}

// hookPayloadForRebuild is a minimal payload struct for RebuildInMemoryState.
type hookPayloadForRebuild struct {
	ToolInput       json.RawMessage `json:"tool_input"`
	PermSuggestions json.RawMessage `json:"permission_suggestions"`
	MsgText         string          `json:"msg_text"` // stored in enriched payload for rebuild
}

// cleanupAskState cleans up bot memory state and freezes TG buttons.
func cleanupAskState(
	bot *tele.Bot,
	toolNotifs *stores.ToolNotifyStore,
	pendingFiles *stores.PendingFileStore,
	msgID int,
	uuid string,
	reason string,
) {
	if entry, ok := toolNotifs.Get(msgID); ok && !entry.Resolved {
		toolNotifs.MarkResolved(msgID)
		editMsg := &tele.Message{ID: msgID, Chat: &tele.Chat{ID: entry.ChatID}}
		RetryEdit(bot, editMsg, entry.MsgText, BuildFrozenMarkup(entry, "❌ Cancelled"), tele.ModeHTML)
	}
	pendingFiles.Remove(msgID)
	logger.Info(fmt.Sprintf("Stale pending cleanup: msg_id=%d uuid=%s reason=%s", msgID, uuid, reason))
}

// ExtractTmuxTargetFromText extracts tmux target from notification text.
func ExtractTmuxTargetFromText(text string) (*injector.TmuxTarget, error) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "📟 ") {
			raw := strings.TrimPrefix(line, "📟 ")
			target, err := injector.ParseTarget(raw)
			if err != nil {
				return nil, err
			}
			return &target, nil
		}
	}
	return nil, fmt.Errorf("no tmux target found")
}

// ReadAssistantTexts reads all assistant text entries from a transcript JSONL file.
func ReadAssistantTexts(transcriptPath string) []string {
	content, err := os.ReadFile(transcriptPath)
	if err != nil {
		return nil
	}
	var texts []string
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry map[string]interface{}
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		typ, _ := entry["type"].(string)
		// CC format: {type: "assistant", message: {content: [{type: "text", text: "..."}]}}
		if typ == "assistant" {
			if model, _ := entry["model"].(string); model == "<synthetic>" {
				continue
			}
			msg, _ := entry["message"].(map[string]interface{})
			if msg == nil {
				continue
			}
			contentArr, _ := msg["content"].([]interface{})
			if extracted := extractTextPartsFromIface(contentArr); extracted != "" {
				texts = append(texts, extracted)
			}
			continue
		}
		// Codex format: {type: "response_item", payload: {role: "assistant", content: [{type: "output_text", text: "..."}]}}
		if typ == "response_item" {
			payload, _ := entry["payload"].(map[string]interface{})
			if payload == nil {
				continue
			}
			if role, _ := payload["role"].(string); role != "assistant" {
				continue
			}
			contentArr, _ := payload["content"].([]interface{})
			if extracted := extractTextPartsFromIface(contentArr); extracted != "" {
				texts = append(texts, extracted)
			}
			continue
		}
	}
	return texts
}

// extractTextPartsFromIface extracts text from content arrays in both CC and Codex formats.
func extractTextPartsFromIface(contentArr []interface{}) string {
	if contentArr == nil {
		return ""
	}
	var textParts []string
	for _, c := range contentArr {
		cMap, _ := c.(map[string]interface{})
		if cMap == nil {
			continue
		}
		cType, _ := cMap["type"].(string)
		// CC: type="text", Codex: type="output_text"
		if cType == "text" || cType == "output_text" {
			if text, ok := cMap["text"].(string); ok {
				textParts = append(textParts, text)
			}
		}
	}
	if len(textParts) == 0 {
		return ""
	}
	joined := strings.Join(textParts, "\n")
	if joined == "No response requested." {
		return ""
	}
	return joined
}
