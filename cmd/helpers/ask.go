package helpers

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// DoRespondAsk responds to AskUserQuestion: handleStalePending + writePendingAnswer + edit + recordPending.
func DoRespondAsk(
	bot *tele.Bot,
	toolNotifs *stores.ToolNotifyStore,
	pendingFiles *stores.PendingFileStore,
	reactionTracker *stores.ReactionTrackerStore,
	msgID int,
	answers map[string]string,
	frozenLabel string,
) error {
	uuid, ok := pendingFiles.Get(msgID)
	if !ok {
		return fmt.Errorf("pending file not found")
	}
	if HandleStalePending(msgID, uuid, func(mid int, u string, reason string) {
		cleanupAskState(bot, toolNotifs, pendingFiles, mid, u, reason)
	}) {
		return fmt.Errorf("hook dead (stale pending)")
	}
	path := filepath.Join(PendingDir(), uuid+".json")
	pf, err := ReadPendingFile(path)
	if err != nil {
		cleanupAskState(bot, toolNotifs, pendingFiles, msgID, uuid, "file missing on respond")
		return fmt.Errorf("question expired")
	}
	ccOutput := BuildAskCCOutput(pf.Payload, answers)
	if err := WritePendingAnswer(uuid, ccOutput); err != nil {
		logger.Error(fmt.Sprintf("Failed to write pending answer: %v", err))
		return fmt.Errorf("failed to save answer")
	}
	if entry, entryOk := toolNotifs.Get(msgID); entryOk {
		toolNotifs.MarkResolved(msgID)
		editMsg := &tele.Message{ID: msgID, Chat: &tele.Chat{ID: entry.ChatID}}
		RetryEdit(bot, editMsg, entry.MsgText, BuildFrozenMarkup(entry, frozenLabel), tele.ModeHTML)
		reactionTracker.RecordPending(entry.TmuxTarget, entry.ChatID, msgID)
	}
	logger.Info(fmt.Sprintf("AskUserQuestion responded: msg_id=%d uuid=%s answers=%v", msgID, uuid, answers))
	return nil
}

// DoCancelAsk cancels an AskUserQuestion: disk write + ESC + resolve + edit TG msg.
func DoCancelAsk(
	bot *tele.Bot,
	toolNotifs *stores.ToolNotifyStore,
	pendingFiles *stores.PendingFileStore,
	extractTarget func(string) (*injector.TmuxTarget, error),
	msgID int,
) string {
	uuid, uuidOk := pendingFiles.Get(msgID)
	if uuidOk {
		path := filepath.Join(PendingDir(), uuid+".json")
		pf, err := ReadPendingFile(path)
		if err == nil {
			pf.Status = "cancelled"
			WritePendingFile(path, pf)
		}
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

// DoChatAsk handles chat mode for AskUserQuestion: handleStalePending + write __chat answer + edit.
func DoChatAsk(
	bot *tele.Bot,
	toolNotifs *stores.ToolNotifyStore,
	pendingFiles *stores.PendingFileStore,
	reactionTracker *stores.ReactionTrackerStore,
	msgID int,
) error {
	uuid, ok := pendingFiles.Get(msgID)
	if !ok {
		return fmt.Errorf("pending file not found")
	}
	if HandleStalePending(msgID, uuid, func(mid int, u string, reason string) {
		cleanupAskState(bot, toolNotifs, pendingFiles, mid, u, reason)
	}) {
		return fmt.Errorf("question expired")
	}
	path := filepath.Join(PendingDir(), uuid+".json")
	pf, err := ReadPendingFile(path)
	if err != nil {
		cleanupAskState(bot, toolNotifs, pendingFiles, msgID, uuid, "file missing on chat button")
		return fmt.Errorf("question expired")
	}
	answers := map[string]string{"__chat": "true"}
	ccOutput := BuildAskCCOutput(pf.Payload, answers)
	if err := WritePendingAnswer(uuid, ccOutput); err != nil {
		logger.Error(fmt.Sprintf("Failed to write pending answer: %v", err))
		return fmt.Errorf("failed to save answer")
	}
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
	Bot             *tele.Bot
	ToolNotifs      *stores.ToolNotifyStore
	PendingFiles    *stores.PendingFileStore
	PendingPerms    *stores.PendingPermStore
	InjectQueue     *stores.InjectQueueStore
	InjectConfirm   *stores.InjectConfirmStore
	StopCooldown    *stores.StopCooldownStore
	ReactionTracker *stores.ReactionTrackerStore
	SessionState    *stores.SessionStateStore
	ResolveChat     func(string) (*tele.Chat, string, int)
	FormatPaneID    func(string) string
}

// SafeInjectText checks for pending AskUserQuestion/PermissionRequest on the target pane.
// If AskUserQuestion is pending, answers it with the text and returns. Otherwise injects text directly.
func SafeInjectText(p SafeInjectTextParams, tmuxTarget string, text string, submit ...bool) error {
	target, err := injector.ParseTarget(tmuxTarget)
	if err != nil {
		return err
	}
	// PRE-INJECT: check if there's a pending AskUserQuestion
	_, _, hasAskQ := p.ToolNotifs.FindByTmuxTarget(tmuxTarget)
	if IsSessionRunning(tmuxTarget) && !hasAskQ {
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
			notifyText := fmt.Sprintf("⏳ Queued [%s] (%d)\n📟 %s\n──────\n%s\n──────", queueID, count, p.FormatPaneID(tmuxTarget), strings.Join(allTexts, "\n"))
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
		return nil
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
		if HandleStalePending(msgID, uuid, func(mid int, u string, reason string) {
			cleanupAskState(p.Bot, p.ToolNotifs, p.PendingFiles, mid, u, reason)
		}) {
			continue
		}
		path := filepath.Join(PendingDir(), uuid+".json")
		pf, pfErr := ReadPendingFile(path)
		if pfErr != nil {
			p.ToolNotifs.MarkResolved(msgID)
			continue
		}
		answers := make(map[string]string)
		if len(entry.Questions) > 0 {
			answers[entry.Questions[0].QuestionText] = text
		}
		ccOutput := BuildAskCCOutput(pf.Payload, answers)
		if writeErr := WritePendingAnswer(uuid, ccOutput); writeErr != nil {
			logger.Error(fmt.Sprintf("safeInjectText: failed to write answer: %v", writeErr))
		}
		p.ToolNotifs.MarkResolved(msgID)
		editMsg := &tele.Message{ID: msgID, Chat: &tele.Chat{ID: entry.ChatID}}
		RetryEdit(p.Bot, editMsg, entry.MsgText, BuildFrozenMarkup(entry, "✅ Custom reply"), tele.ModeHTML)
		logger.Info(fmt.Sprintf("safeInjectText: answered AskUserQuestion msg_id=%d uuid=%s text=%s", msgID, uuid, TruncateStr(text, 200)))
		return nil
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
			notifyText := fmt.Sprintf("⏳ Queued [%s] (%d)\n📟 %s\n🔒 PermissionRequest pending\n──────\n%s\n──────", queueID, count, p.FormatPaneID(tmuxTarget), strings.Join(allTexts, "\n"))
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
		return nil
	}
	// Wait for Stop event cooldown before injecting
	p.StopCooldown.WaitIfNeeded(tmuxTarget, 3*time.Second)
	shouldSubmit := len(submit) == 0 || submit[0]
	ch := p.InjectConfirm.Register(tmuxTarget)
	if err := injector.InjectText(target, text, shouldSubmit); err != nil {
		p.InjectConfirm.Cancel(tmuxTarget)
		return err
	}
	if !shouldSubmit {
		p.InjectConfirm.Cancel(tmuxTarget)
		return nil
	}
	select {
	case <-ch:
		return nil
	case <-time.After(10 * time.Second):
		p.InjectConfirm.Cancel(tmuxTarget)
		logger.Debug(fmt.Sprintf("safeInjectText: inject confirmation timeout for target=%s", tmuxTarget))
		return nil
	}
}

// RebuildInMemoryState reconstructs in-memory maps from a status=sent pending file.
func RebuildInMemoryState(
	toolNotifs *stores.ToolNotifyStore,
	pendingFiles *stores.PendingFileStore,
	pendingPerms *stores.PendingPermStore,
	pf *PendingFile,
	formatPaneID func(string) string,
) error {
	var p hookPayloadForRebuild
	if err := json.Unmarshal(pf.Payload, &p); err != nil {
		return fmt.Errorf("unmarshal payload: %w", err)
	}
	tmuxTarget := formatPaneID(pf.TmuxTarget)
	if pf.ToolName == "AskUserQuestion" {
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
		toolNotifs.Store(pf.TgMsgID, &stores.ToolNotifyEntry{
			TmuxTarget: tmuxTarget, ToolName: "AskUserQuestion",
			Questions: qMetas, ChatID: pf.TgChatID, MsgText: pf.TgMsgText,
			PendingUUID: pf.UUID,
		})
		pendingFiles.Store(pf.TgMsgID, pf.UUID)
		logger.Info(fmt.Sprintf("scanPendingDir: rebuilt AskUserQuestion state: msg_id=%d questions=%d tmux=%s content=%s uuid=%s", pf.TgMsgID, len(askInput.Questions), tmuxTarget, contentSummary, pf.UUID))
		return nil
	}
	// PermissionRequest: rebuild pendingPerms
	var suggestions []json.RawMessage
	json.Unmarshal(p.PermSuggestions, &suggestions)
	suggestionsRaw, _ := json.Marshal(suggestions)
	pendingPerms.Create(pf.TgMsgID, tmuxTarget, suggestionsRaw, pf.TgMsgText, pf.TgChatID, pf.UUID)
	pendingFiles.Store(pf.TgMsgID, pf.UUID)
	logger.Info(fmt.Sprintf("scanPendingDir: rebuilt PermissionRequest state: msg_id=%d tool=%s tmux=%s uuid=%s", pf.TgMsgID, pf.ToolName, tmuxTarget, pf.UUID))
	return nil
}

// hookPayloadForRebuild is a minimal payload struct for RebuildInMemoryState.
type hookPayloadForRebuild struct {
	ToolInput       json.RawMessage `json:"tool_input"`
	PermSuggestions json.RawMessage `json:"permission_suggestions"`
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
