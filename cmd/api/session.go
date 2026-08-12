package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Seraphli/tg-cli/cmd/handlers"
	"github.com/Seraphli/tg-cli/cmd/helpers"
	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/cmd/types"
	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/logger"
	"github.com/Seraphli/tg-cli/internal/notify"
	tele "gopkg.in/telebot.v3"
)

func registerSession(mux *http.ServeMux, bs *types.BotState) {
	bot := bs.Bot
	mux.HandleFunc("/session/idle", func(w http.ResponseWriter, r *http.Request) {
		targetFilter := r.URL.Query().Get("target")
		sessions := bs.SessionState.All()

		type sessionIdleEntry struct {
			Target string `json:"target"`
			Idle   bool   `json:"idle"`
		}
		result := make(map[string]sessionIdleEntry)
		allIdle := len(sessions) > 0 // empty sessions = not idle

		for sid, info := range sessions {
			if targetFilter != "" && info.TmuxTarget != targetFilter {
				continue
			}
			running := helpers.IsSessionRunning(bs.HookRunning, info.TmuxTarget)
			if running {
				allIdle = false
			}
			result[sid] = sessionIdleEntry{Target: info.TmuxTarget, Idle: !running}
		}

		// If a target was specified but no registered session matched, check the
		// tmux target directly (works before the session is registered, e.g. during
		// codex startup). "idle" here means a known CLI (cc/codex) is up and not
		// busy — NOT merely "nothing busy". IsSessionRunning returns false both when
		// a CLI is idle AND when no CLI is running yet (empty title / unknown
		// backend), so gate on a detected backend + non-empty title; otherwise a
		// not-yet-started pane would be falsely reported idle and callers polling for
		// readiness (start_codex) would proceed too early.
		if targetFilter != "" && len(result) == 0 {
			title := helpers.GetPaneTitle(targetFilter)
			backend := helpers.DetectBackend(targetFilter)
			var idle bool
			if title != "" && backend != "" {
				idle = !helpers.IsSessionRunning(bs.HookRunning, targetFilter)
			} else if title != "" {
				idle = !helpers.TitleIsBusy("codex", title)
			}
			allIdle = idle
			result[targetFilter] = sessionIdleEntry{Target: targetFilter, Idle: idle}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"idle":     allIdle,
			"sessions": result,
		})
	})
	mux.HandleFunc("/session/name", func(w http.ResponseWriter, r *http.Request) {
		sessionID := r.URL.Query().Get("session_id")
		name := r.URL.Query().Get("name")
		ok, errMsg := bs.SessionState.SetName(sessionID, name)
		if !ok {
			http.Error(w, errMsg, 400)
			return
		}
		bs.InjectQueue.ClearDeadTargets(injector.TargetExists)
		logger.Info(fmt.Sprintf("Session name set via API: session=%s name=%s", sessionID, name))
		w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/pending/cancel", func(w http.ResponseWriter, r *http.Request) {
		uuid := r.URL.Query().Get("uuid")
		if uuid == "" {
			http.Error(w, "missing uuid", 400)
			return
		}
		// Get snapshot before CAS to access TmuxTarget for ESC
		snap, snapFound := bs.PendingWait.GetSnapshot(uuid)
		if !snapFound {
			w.WriteHeader(200)
			return
		}
		// After CAS win: send ESC to unblock the pane
		won, _, _ := bs.PendingWait.ResolveIfUnresolved(uuid, stores.WaitEvent{Type: "cancel"})
		if won {
			// Send ESC to unblock the pane
			if snap.TmuxTarget != "" {
				if t, err := injector.ParseTarget(snap.TmuxTarget); err == nil {
					injector.SendKeys(t, "Escape")
				}
			}
			logger.Info(fmt.Sprintf("Permission cancelled: msg_id=%d uuid=%s tool=%s", snap.MsgID, uuid, snap.ToolName))
			// Build frozen markup from snapshot and TryEnqueue EDIT
			capturedLabel := "❌ Cancelled"
			var frozenMarkup *tele.ReplyMarkup
			if snap.ToolName == "AskUserQuestion" {
				frozenMarkup = helpers.BuildFrozenMarkup(snap.Questions, capturedLabel)
			} else {
				sugLabel, _ := helpers.ParseSuggestionLabel(snap.PermSuggestions)
				frozenMarkup = helpers.BuildFrozenPermMarkup(capturedLabel, sugLabel)
			}
			if frozenMarkup != nil {
				capturedMarkup := frozenMarkup
				capturedRich := snap.Rich
				bs.PendingMsgStore.EditOrDefer(uuid, func(msgID int, chatID int64, editMsgText string, topicID int) {
					editMsg := &tele.Message{ID: msgID, Chat: &tele.Chat{ID: chatID}}
					_, err := helpers.RetryFreezeEditAuto(bot, editMsg, capturedRich, editMsgText, capturedMarkup)
					if err != nil {
						logger.Error(fmt.Sprintf("/pending/cancel EDIT failed msg_id=%d uuid=%s err=%v", msgID, uuid, err))
					} else {
						logger.Info(fmt.Sprintf("/pending/cancel EDIT completed msg_id=%d uuid=%s label=%s", msgID, uuid, capturedLabel))
					}
				})
			}
		}
		w.WriteHeader(200)
	})
	mux.HandleFunc("/session/list", func(w http.ResponseWriter, r *http.Request) {
		sessions := bs.SessionState.All()
		type sessionListItem struct {
			ID             string `json:"id"`
			Name           string `json:"name"`
			Target         string `json:"target"`
			CWD            string `json:"cwd"`
			ProjectDir     string `json:"project_dir"`
			Running        bool   `json:"running"`
			TranscriptPath string `json:"transcript_path,omitempty"`
		}
		items := make([]sessionListItem, 0, len(sessions))
		for sid, info := range sessions {
			items = append(items, sessionListItem{
				ID:             sid,
				Name:           info.Name,
				Target:         info.TmuxTarget,
				CWD:            info.CWD,
				ProjectDir:     info.ProjectDir,
				Running:        helpers.IsSessionRunning(bs.HookRunning, info.TmuxTarget),
				TranscriptPath: info.TranscriptPath,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"sessions": items})
	})
	mux.HandleFunc("/session/log", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		if name == "" {
			http.Error(w, "name required", http.StatusBadRequest)
			return
		}
		linesParam := r.URL.Query().Get("lines")
		maxLines := 20
		if linesParam != "" {
			if n, err := strconv.Atoi(linesParam); err == nil && n > 0 {
				maxLines = n
			}
		}
		noTools := r.URL.Query().Get("no_tools") == "true" || r.URL.Query().Get("no_tools") == "1"

		// Tools that are filtered out when no_tools=true (AskUserQuestion is NOT in this list)
		filteredTools := map[string]bool{
			"Bash": true, "Read": true, "Write": true, "Edit": true,
			"Glob": true, "Grep": true, "Agent": true, "WebFetch": true,
			"WebSearch": true, "NotebookEdit": true,
		}

		// Find session by name and get session ID for context lookup
		var info *stores.SessionInfo
		var sessionID string
		for sid, si := range bs.SessionState.All() {
			if si.Name == name {
				cp := si
				info = &cp
				sessionID = sid
				break
			}
		}
		if info == nil {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}

		// Use stored transcript path directly (works for both CC and Codex)
		jsonlPath := info.TranscriptPath
		if jsonlPath == "" {
			home, _ := os.UserHomeDir()
			jsonlPath = filepath.Join(home, ".claude", "projects", helpers.ProjectSlug(info.CWD), sessionID+".jsonl")
		}
		if _, err := os.Stat(jsonlPath); err != nil {
			http.Error(w, "transcript not found", http.StatusNotFound)
			return
		}
		type fileEntry struct {
			path    string
			modTime time.Time
		}
		jsonlFiles := []fileEntry{{path: jsonlPath, modTime: time.Now()}}

		// formatToolDetail formats human-readable tool details based on tool name and input
		formatToolDetail := func(toolName string, input map[string]interface{}) string {
			truncate := func(s string, n int) string {
				if len(s) > n {
					return s[:n] + "..."
				}
				return s
			}
			switch toolName {
			case "AskUserQuestion":
				// AskUserQuestion input has a "questions" array
				if questions, ok := input["questions"].([]interface{}); ok && len(questions) > 0 {
					var parts []string
					for qi, q := range questions {
						qMap, _ := q.(map[string]interface{})
						if qMap == nil {
							continue
						}
						header, _ := qMap["header"].(string)
						question, _ := qMap["question"].(string)
						line := fmt.Sprintf("Q%d %q: %s", qi+1, header, question)
						if opts, ok := qMap["options"].([]interface{}); ok {
							for i, o := range opts {
								oMap, _ := o.(map[string]interface{})
								if oMap != nil {
									label, _ := oMap["label"].(string)
									line += fmt.Sprintf("\n  %d. %s", i+1, label)
								} else {
									line += fmt.Sprintf("\n  %d. %v", i+1, o)
								}
							}
						}
						parts = append(parts, line)
					}
					return strings.Join(parts, "\n")
				}
				return "AskUserQuestion"
			case "Bash":
				cmd, _ := input["command"].(string)
				desc, _ := input["description"].(string)
				if desc != "" {
					return truncate(cmd, 200) + "\nℹ️ " + desc
				}
				return truncate(cmd, 200)
			case "Edit":
				fp, _ := input["file_path"].(string)
				oldStr, _ := input["old_string"].(string)
				newStr, _ := input["new_string"].(string)
				detail := fp
				if oldStr != "" {
					detail += "\nOld: " + truncate(oldStr, 80)
				}
				if newStr != "" {
					detail += "\nNew: " + truncate(newStr, 80)
				}
				return detail
			case "Write", "Read":
				fp, _ := input["file_path"].(string)
				if fp == "" {
					fp, _ = input["path"].(string) // pi's field is `path`
				}
				return fp
			case "ls": // pi (lowercase, no CC analogue): the listed directory path
				p, _ := input["path"].(string)
				return p
			case "Glob", "Grep":
				pat, _ := input["pattern"].(string)
				return pat
			case "Agent":
				desc, _ := input["description"].(string)
				return truncate(desc, 200)
			default:
				return toolName
			}
		}

		var result []helpers.TranscriptLogEntry

		for _, jf := range jsonlFiles {
			if len(result) >= maxLines {
				break
			}
			f, err := os.Open(jf.path)
			if err != nil {
				continue
			}
			var fileEntries []helpers.TranscriptLogEntry
			if info.Backend == "codex" {
				fileEntries = helpers.ParseCodexTranscript(f, noTools, filteredTools, formatToolDetail)
			} else if info.Backend == "pi" {
				fileEntries = helpers.ParsePiTranscript(f, noTools, filteredTools, formatToolDetail)
			} else {
				fileEntries = helpers.ParseCCTranscript(f, noTools, filteredTools, formatToolDetail)
			}
			f.Close()
			result = append(result, fileEntries...)
		}

		// Trim to maxLines (keep the most recent entries)
		if len(result) > maxLines {
			result = result[len(result)-maxLines:]
		}

		// Build context usage strings
		ctxPct := 0
		ctxUsed := ""
		ctxTotal := ""
		if usedPct, usedTokens, windowSize, ok := helpers.ReadContextUsage(sessionID); ok {
			ctxPct = usedPct
			formatK := func(n int) string {
				return fmt.Sprintf("%.1fk", float64(n)/1000)
			}
			ctxUsed = formatK(usedTokens)
			ctxTotal = formatK(windowSize)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"target":        info.TmuxTarget,
			"context_pct":   ctxPct,
			"context_used":  ctxUsed,
			"context_total": ctxTotal,
			"messages":      result,
		})
	})
	mux.HandleFunc("/session/send", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Name     string `json:"name"`
			Text     string `json:"text"`
			From     string `json:"from"`
			NoHeader bool   `json:"noHeader"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.From == "" {
			http.Error(w, "from required", http.StatusBadRequest)
			return
		}
		info := bs.SessionState.FindByName(req.Name)
		if info == nil {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		injectText := req.Text
		if !req.NoHeader {
			injectText = fmt.Sprintf("---\n💬 Message from agent [%s]\n---\n%s", req.From, req.Text)
		}
		p := buildSafeInjectParams(bs)
		injErr := helpers.SafeInjectText(p, info.TmuxTarget, injectText)
		status, deliveryStatus, doNotify := sessionSendResult(injErr)
		if !doNotify {
			// Hard pre-delivery failure: no notification, report the error.
			http.Error(w, injErr.Error(), status)
			return
		}
		logger.Info(fmt.Sprintf("Session send via API: name=%s target=%s from=%s noHeader=%t delivery_status=%q text=%s injectText=%q", req.Name, info.TmuxTarget, req.From, req.NoHeader, deliveryStatus, req.Text, injectText))
		// Send TG notification to the target session's chat. The neutral SessionSend header is built by
		// BuildNotificationText; the raw req.Text is paginated via SplitRichLegacyBodyPages (paired
		// rich/legacy BODY chunks). The rich payload gets the <hr/> boundary (InsertRichHr); legacy does not.
		chat, _, topicID := helpers.ResolveChat(bs.SessionState, info.TmuxTarget)
		if chat != nil {
			cfg, _ := config.LoadAppConfig()
			richMax := cfg.RichMaxRunes
			if richMax <= 0 {
				richMax = 30000
			}
			baseND := notify.NotificationData{
				Event:          "SessionSend",
				SendFrom:       req.From,
				SendNoHeader:   req.NoHeader,
				TmuxTarget:     info.TmuxTarget,
				ContextUsedPct: -1,
				DeliveryStatus: deliveryStatus,
			}
			chunks, legacyChunks, err := helpers.SplitRichLegacyBodyPages(req.Text, baseND, richMax)
			if err != nil {
				// Minimal, independently bounded fallback: fixed message + sender; no pagination, no loop.
				fallback := fmt.Sprintf("💬 CLI Send from %s: message too large to render", req.From) + notify.DeliveryStatusTag(deliveryStatus)
				helpers.RetrySendRich(bot, chat, fallback, helpers.RichSendOpts{TopicID: topicID, LegacyHTML: fallback})
				logger.Info(fmt.Sprintf("Session send fallback: target=%s from=%s err=%v", req.Name, req.From, err))
			} else {
				pageND := func(page int) notify.NotificationData {
					nd := baseND
					if len(chunks) > 1 {
						nd.Page = page
						nd.TotalPages = len(chunks)
					}
					return nd
				}
				nd1 := pageND(1)
				nd1.Body = chunks[0]
				richText := helpers.InsertRichHr(notify.BuildNotificationText(nd1))
				nd1Legacy := pageND(1)
				nd1Legacy.Body = legacyChunks[0]
				legacyText := notify.BuildNotificationText(nd1Legacy)
				opts := helpers.RichSendOpts{TopicID: topicID, LegacyHTML: legacyText}
				if len(chunks) > 1 {
					opts.Markup = helpers.BuildPageKeyboard(1, len(chunks))
				}
				sent, serr := helpers.RetrySendRich(bot, chat, richText, opts)
				if serr == nil && len(chunks) > 1 {
					bs.Pages.Store(sent.ID, "", &stores.PageEntry{
						Chunks:         chunks,
						LegacyChunks:   legacyChunks,
						Event:          "SessionSend",
						TmuxTarget:     info.TmuxTarget,
						ContextUsedPct: -1,
						SendFrom:       req.From,
						SendNoHeader:   req.NoHeader,
						DeliveryStatus: deliveryStatus,
						ChatID:         chat.ID,
						Rich:           true,
					})
				}
				logger.Info(fmt.Sprintf("Session send notification: target=%s from=%s pages=%d text=%s", req.Name, req.From, len(chunks), req.Text))
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(sessionSendBody(deliveryStatus)))
	})
	mux.HandleFunc("/session/exit", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		info := bs.SessionState.FindByName(req.Name)
		if info == nil {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		target := info.TmuxTarget
		// Cancel pending tool wait via PendingWait.FindByTmuxTarget (target-based path)
		if pwSnap, hasPW := bs.PendingWait.FindByTmuxTarget(target); hasPW {
			switch pwSnap.ToolName {
			default:
				// CancelPermBySnapshot: send ESC + ResolveIfUnresolved + EditOrDefer (any non-AskQ tool is a PermReq)
				helpers.CancelPermBySnapshot(bot, bs.PendingWait, bs.PendingMsgStore, notify.FormatPaneID, *pwSnap)
				if t, err := injector.ParseTarget(target); err == nil {
					injector.SendKeys(t, "Escape")
				}
			case "AskUserQuestion":
				// CancelAskBySnapshot: send ESC + ResolveIfUnresolved + EditOrDefer for AskQ
				helpers.CancelAskBySnapshot(bot, bs.PendingWait, bs.PendingMsgStore, notify.FormatPaneID, *pwSnap)
			}
		}
		// Wait for pending cleanup
		time.Sleep(1 * time.Second)
		// Inject /exit command
		t, err := injector.ParseTarget(target)
		if err != nil {
			http.Error(w, "invalid target: "+err.Error(), http.StatusInternalServerError)
			return
		}
		// Store PendingExitKill BEFORE inject to avoid race with SessionEnd hook
		bs.PendingExitKill.Store(target, true)
		if err := helpers.QueuedInject(bs.SessionEvents, bs.SessionState, t, "/exit"); err != nil {
			bs.PendingExitKill.Delete(target)
			http.Error(w, "inject failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		logger.Info(fmt.Sprintf("Session exit via API: name=%s target=%s", req.Name, target))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/session/new", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Session string `json:"session"`
			WorkDir string `json:"workdir"`
			Command string `json:"command"`
			Name    string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		chatID, err := strconv.ParseInt(bs.Creds.PairingAllow.DefaultChatID, 10, 64)
		if err != nil {
			http.Error(w, "no default chat configured", http.StatusBadRequest)
			return
		}
		state := &handlers.LaunchState{
			SessionName: req.Session,
			WorkDir:     req.WorkDir,
			Command:     req.Command,
			AgentName:   req.Name,
			ChatID:      chatID,
			UUID:        handlers.GenerateLaunchUUID(),
		}
		if req.Session != "" && req.WorkDir != "" {
			go handlers.ExecuteLaunch(bs, bot, chatID, state)
		} else if req.Session == "" {
			handlers.AskSessionName(bs, bot, chatID, state)
		} else {
			handlers.AskWorkDir(bs, bot, chatID, state)
		}
		logger.Info(fmt.Sprintf("Session new via API: session=%s workdir=%s name=%s uuid=%s", req.Session, req.WorkDir, req.Name, state.UUID))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "uuid": state.UUID})
	})
	mux.HandleFunc("/session/watch", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		if name == "" {
			http.Error(w, "name required", http.StatusBadRequest)
			return
		}
		ch := bs.SessionWatch.Register(name)
		select {
		case evt := <-ch:
			logger.Info(fmt.Sprintf("Session watch event: agent=%s event=%s", name, evt.Event))
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(evt)
		case <-r.Context().Done():
			bs.SessionWatch.Cancel(name, ch)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "timeout"})
		}
	})
}

// sessionSendResult classifies a SafeInjectText outcome for /session/send. A nil error or a
// post-paste "text reached the pane" sentinel (ErrInjectNotConfirmed / ErrSubmitAfterPaste) is a
// SOFT result: HTTP 200 + a delivery-status annotation, and the TG notification is still sent (the
// message WAS delivered; a 500 would invite a dangerous re-send). Any pre-delivery error is HARD:
// HTTP 500, no notification.
func sessionSendResult(injErr error) (status int, deliveryStatus string, doNotify bool) {
	if injErr == nil {
		return http.StatusOK, "", true
	}
	if errors.Is(injErr, helpers.ErrInjectNotConfirmed) {
		return http.StatusOK, "unconfirmed", true
	}
	if errors.Is(injErr, injector.ErrSubmitAfterPaste) {
		return http.StatusOK, "submit_failed", true
	}
	return http.StatusInternalServerError, "", false
}

// sessionSendBody builds the /session/send JSON response. delivery_status is omitted when empty so
// the normal-path body stays byte-identical to the historical {"ok":true}.
func sessionSendBody(deliveryStatus string) string {
	if deliveryStatus == "" {
		return `{"ok":true}`
	}
	return fmt.Sprintf(`{"ok":true,"delivery_status":%q}`, deliveryStatus)
}

// buildSafeInjectParams constructs SafeInjectTextParams from BotState.
func buildSafeInjectParams(bs *types.BotState) helpers.SafeInjectTextParams {
	return helpers.SafeInjectTextParams{
		Bot:              bs.Bot,
		PendingWait:      bs.PendingWait,
		InjectQueue:      bs.InjectQueue,
		InjectConfirm:    bs.InjectConfirm,
		StopCooldown:     bs.StopCooldown,
		ReactionTracker:  bs.ReactionTracker,
		SessionState:     bs.SessionState,
		HookRunning:      bs.HookRunning,
		HookSessionLocks: &bs.HookSessionLocks,
		SessionEvents:    bs.SessionEvents,
		PendingMsgStore:  bs.PendingMsgStore,
		ResolveChat: func(t string) (*tele.Chat, string, int) {
			return helpers.ResolveChat(bs.SessionState, t)
		},
		FormatPaneID: notify.FormatPaneID,
	}
}
