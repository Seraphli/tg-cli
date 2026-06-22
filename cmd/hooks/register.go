package hooks

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Seraphli/tg-cli/cmd/helpers"
	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/cmd/types"
	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/logger"
	"github.com/Seraphli/tg-cli/internal/notify"
	tele "gopkg.in/telebot.v3"
)

// Register registers all POST /hook/* and /pending/* endpoints on mux.
func Register(mux *http.ServeMux, bs *types.BotState, port int, cb Callbacks) {
	mux.HandleFunc("/pending/connect", pendingConnectHandler(bs, cb))
	mux.HandleFunc("/hook/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.NotFound(w, r)
			return
		}
		event := strings.TrimPrefix(r.URL.Path, "/hook/")
		p, raw, err := helpers.ParseHookPayload(r)
		if err != nil {
			http.Error(w, "bad request", 400)
			return
		}
		// Strip socket suffix so internal stores use bare pane IDs (e.g. %859 not %859@/tmp/...)
		p.TmuxTarget = notify.FormatPaneID(p.TmuxTarget)
		if event == "MessageDisplay" {
			handleMessageDisplay(bs, cb, p)
			w.WriteHeader(200)
			return
		}
		logger.Info(fmt.Sprintf("Raw hook payload [%s]: %s", event, string(raw)))
		// Re-register session on any hook event (survives bot restart)
		// Exclude SessionStart (handled inside switch after filter) and SessionEnd
		if event != "SessionEnd" && event != "SessionStart" && p.SessionID != "" && p.TmuxTarget != "" {
			bs.SessionState.Add(p.SessionID, p.TmuxTarget, p.CWD, p.TranscriptPath)
			if p.Backend != "" {
				bs.SessionState.SetBackend(p.SessionID, p.Backend)
			}
		}
		// Dispatch hook handling through the session event queue to serialize per-session events
		bs.SessionEvents.Dispatch(p.SessionID, "hook:"+event, func() error {
			// Use stored session CWD for routing to avoid drift from cd commands in CC
			cwdForRoute := p.CWD
			hookInfo := bs.SessionState.FindInfoByTarget(p.TmuxTarget)
			if hookInfo != nil && hookInfo.CWD != "" {
				cwdForRoute = hookInfo.CWD
			}
			hookAgentName := ""
			if hookInfo != nil {
				hookAgentName = hookInfo.Name
			}
			chat, chatID, hookTopicID := cb.ResolveChat(bs, p.TmuxTarget)
			switch event {
			case "SessionStart":
				// Skip temp usage sessions
				if p.TmuxTarget != "" {
					if target, err := injector.ParseTarget(p.TmuxTarget); err == nil {
						sNameBytes, _ := injector.TmuxCmd(target, "display-message", "-p", "-t", target.PaneID, "#{session_name}").Output()
						if strings.HasPrefix(strings.TrimSpace(string(sNameBytes)), "tg-cli-usage-") {
							break
						}
					}
				}
				if chat == nil || p.TmuxTarget == "" {
					return nil
				}
				var body string
				if p.Source == "resume" && p.TranscriptPath != "" {
					body = helpers.ReadLastAssistantText(p.TranscriptPath)
				}
				cb.SendEventNotification(bs, chat, chatID, p.SessionID, "SessionStart", p.Project, cwdForRoute, p.TmuxTarget, body, "", hookAgentName, hookTopicID)
				logger.Info(fmt.Sprintf("Notification sent to chat %s: SessionStart [%s] tmux=%s", chatID, p.Project, p.TmuxTarget))
				// Migrate route BEFORE Add() — Add() removes stale same-pane sessions
				if p.TmuxTarget != "" && p.SessionID != "" {
					creds, credErr := config.LoadCredentials()
					if credErr == nil {
						allSessions := bs.SessionState.All()
						for oldSID, oldInfo := range allSessions {
							if oldInfo.TmuxTarget == p.TmuxTarget && oldSID != p.SessionID {
								if route, ok := creds.NameRouteMap[oldSID]; ok {
									creds.NameRouteMap[p.SessionID] = route
									delete(creds.NameRouteMap, oldSID)
									config.SaveCredentials(creds)
									logger.Info(fmt.Sprintf("Route migrated: old=%s new=%s pane=%s", oldSID[:8], p.SessionID[:8], p.TmuxTarget))
								}
								break
							}
						}
					}
				}
				if p.SessionID != "" && p.TmuxTarget != "" {
					bs.SessionState.Add(p.SessionID, p.TmuxTarget, p.CWD, p.TranscriptPath)
					if p.Backend != "" {
						bs.SessionState.SetBackend(p.SessionID, p.Backend)
					}
					logger.Info(fmt.Sprintf("Session tracked: %s -> %s", p.SessionID, p.TmuxTarget))
				}
			case "SessionEnd":
				// Skip sessions not in sessionState (temp usage sessions filtered at SessionStart were never added)
				if p.SessionID != "" {
					if bs.SessionState.FindInfoByID(p.SessionID) == nil {
						break
					}
				}
				if chat != nil {
					text := notify.BuildNotificationText(notify.NotificationData{
						Event: "SessionEnd", Project: p.Project, CWD: cwdForRoute, TmuxTarget: p.TmuxTarget,
						AgentName: hookAgentName,
					})
					var sessionEndOpts []interface{}
					sessionEndOpts = append(sessionEndOpts, tele.ModeHTML)
					if hookTopicID > 0 {
						sessionEndOpts = append(sessionEndOpts, &tele.SendOptions{ThreadID: hookTopicID})
					}
					helpers.RetrySend(bs.Bot, chat, text, sessionEndOpts...)
					logger.Info(fmt.Sprintf("Notification sent to chat %s: SessionEnd [%s] tmux=%s", chatID, p.Project, p.TmuxTarget))
				}
				// Close all @ channels involving this session
				if hookAgentName != "" {
					closedPeers := bs.AtChannels.CloseAll(hookAgentName)
					for _, peer := range closedPeers {
						peerInfo := bs.SessionState.FindByName(peer)
						if peerInfo == nil {
							continue
						}
						msg := helpers.BuildAtMsg(hookAgentName, peer, "", "session ended, channel closed")
						peerChat, _, peerTopicID := cb.ResolveChat(bs, peerInfo.TmuxTarget)
						if peerChat != nil {
							var sendOpts []interface{}
							if peerTopicID > 0 {
								sendOpts = append(sendOpts, &tele.SendOptions{ThreadID: peerTopicID})
							}
							helpers.RetrySend(bs.Bot, peerChat, msg, sendOpts...)
						}
						// Inject to peer pane (they need to know the channel is gone)
						go func(target, text string) {
							p := helpers.SafeInjectTextParams{
								Bot:              bs.Bot,
								PendingWait:      bs.PendingWait,
								InjectQueue:      bs.InjectQueue,
								InjectConfirm:    bs.InjectConfirm,
								StopCooldown:     bs.StopCooldown,
								ReactionTracker:  bs.ReactionTracker,
								SessionState:     bs.SessionState,
								HookSessionLocks: &bs.HookSessionLocks,
								SessionEvents:    bs.SessionEvents,
								NotifOpQueue:     bs.NotifOpQueue,
								ResolveChat: func(t string) (*tele.Chat, string, int) {
									return cb.ResolveChat(bs, t)
								},
								FormatPaneID: notify.FormatPaneID,
							}
							helpers.SafeInjectText(p, target, text)
						}(peerInfo.TmuxTarget, msg)
					}
				}
				bs.Pages.CleanupSession(p.SessionID)
				bs.CompactTools.Reset(p.SessionID)
				bs.SessionCounts.Cleanup(p.SessionID)
				CancelPendingWaitBySession(bs, p.SessionID) // cancel (resolve + push cancel) any still-pending hook waits for this session
				bs.Streams.EndSession(p.SessionID) // TTL tombstone; straggler deltas after SessionEnd are dropped
				logger.Info(fmt.Sprintf("Cleaned up session %s", p.SessionID))
				// Signal pending upgrade restart
				if ch, ok := bs.PendingUpgradeRestart.Load(p.TmuxTarget); ok {
					select {
					case ch.(chan struct{}) <- struct{}{}:
					default:
					}
				}
				// Kill tmux pane if this session was exited via /session/exit API
				if _, ok := bs.PendingExitKill.LoadAndDelete(p.TmuxTarget); ok {
					target, parseErr := injector.ParseTarget(p.TmuxTarget)
					if parseErr != nil {
						logger.Error(fmt.Sprintf("SessionEnd kill-pane parse failed: target=%s err=%v", p.TmuxTarget, parseErr))
					} else if err := injector.TmuxCmd(target, "kill-pane", "-t", target.PaneID).Run(); err != nil {
						logger.Error(fmt.Sprintf("SessionEnd kill-pane failed: target=%s err=%v", p.TmuxTarget, err))
					} else {
						logger.Info(fmt.Sprintf("SessionEnd kill-pane: target=%s (exit API)", p.TmuxTarget))
					}
				}
			case "UserPromptSubmit":
				CancelPendingWaitBySession(bs, p.SessionID)
				if p.SessionID != "" && p.TranscriptPath != "" {
					lock := bs.SessionCounts.GetLock(p.SessionID)
					lock.Lock()
					texts := helpers.ReadAssistantTexts(p.TranscriptPath)
					bs.SessionCounts.Counts[p.SessionID] = len(texts)
					lock.Unlock()
					logger.Debug(fmt.Sprintf("UserPromptSubmit position: session=%s count=%d", p.SessionID, len(texts)))
				}
				if p.TmuxTarget != "" {
					cb.TypingLog("state: event=UserPromptSubmit target=%s", p.TmuxTarget)
					bs.InjectConfirm.NotifyUserPromptSubmit(p.TmuxTarget, p.Prompt)
				}
				bs.Streams.Rotate(p.SessionID) // clear previous turn's entries for new user turn (keeps ClosedTurns tombstone)
			case "Stop":
				if p.TmuxTarget != "" {
					bs.HookRunning.SetIdle(p.TmuxTarget)
					cb.TypingLog("state: event=Stop target=%s state=idle", p.TmuxTarget)
					bs.StopCooldown.Record(p.TmuxTarget)
				}
				CancelPendingWaitBySession(bs, p.SessionID)
				if chat != nil {
					body := p.LastAssistantMessage
					// Auto-handle rate-limit popup
					if strings.HasPrefix(strings.TrimSpace(body), "You've hit your limit · resets") {
						logger.Info(fmt.Sprintf("Rate-limit detected in Stop body: session=%s target=%s body=%s", p.SessionID, p.TmuxTarget, helpers.TruncateStr(body, 200)))
						if p.TmuxTarget != "" {
							t, parseErr := injector.ParseTarget(p.TmuxTarget)
							if parseErr == nil {
								time.Sleep(2 * time.Second)
								injector.SendKeys(t, "1")
								time.Sleep(500 * time.Millisecond)
								injector.SendKeys(t, "Enter")
								logger.Info(fmt.Sprintf("Rate-limit auto-handled: sent Enter to %s", p.TmuxTarget))
							}
						}
						if chat != nil {
							notifyText := fmt.Sprintf("⚠️ Rate limit detected\n📟 %s\n\nAuto-selected \"Stop and wait\"", notify.FormatPaneID(p.TmuxTarget))
							var sendOpts []interface{}
							if hookTopicID > 0 {
								sendOpts = append(sendOpts, &tele.SendOptions{ThreadID: hookTopicID})
							}
							helpers.RetrySend(bs.Bot, chat, notifyText, sendOpts...)
						}
						break
					}
					// Update session count for consistency with PreToolUse
					if p.SessionID != "" && p.TranscriptPath != "" {
						lock := bs.SessionCounts.GetLock(p.SessionID)
						lock.Lock()
						texts := helpers.ReadAssistantTexts(p.TranscriptPath)
						bs.SessionCounts.Counts[p.SessionID] = len(texts)
						lock.Unlock()
					}
					// Codex uses transcript-based body; CC uses streaming flush
					if p.Backend == "codex" {
						if body != "" {
							cb.SendEventNotification(bs, chat, chatID, p.SessionID, "Stop", p.Project, cwdForRoute, p.TmuxTarget, body, "", hookAgentName, hookTopicID)
						}
					} else {
						cb.StreamFlush(bs, p.SessionID, true) // drain final batch → finalize last 💬→✅ + table images → close turn
					}
					// Forward Stop output to @ channel targets
					if hookAgentName != "" && body != "" {
						cfg, _ := config.LoadAppConfig()
						dn := cfg.DisplayName
						if dn == "" {
							dn = "User"
						}
						atTargets := bs.AtChannels.GetTargets(hookAgentName)
						buffered := bs.AtChannels.FlushBufferEntries(hookAgentName)
						for _, peerName := range atTargets {
							peerInfo := bs.SessionState.FindByName(peerName)
							if peerInfo == nil {
								continue
							}
							var contentLines []string
							for _, entry := range buffered {
								contentLines = append(contentLines, fmt.Sprintf("[%s → %s]: %s", hookAgentName, dn, entry))
							}
							contentLines = append(contentLines, fmt.Sprintf("[%s → %s]: %s", hookAgentName, dn, body))
							content := strings.Join(contentLines, "\n")
							instructions := fmt.Sprintf("`%s` completed a task. Below is the progress update.", hookAgentName)
							msg := helpers.BuildAtMsg(hookAgentName, peerName, instructions, content)
							logger.Info(fmt.Sprintf("@ forward: %s → %s body_len=%d content=%s", hookAgentName, peerName, len(content), helpers.TruncateStr(content, 200)))
							go func(target, text, instr, cnt, peer string) {
								p := helpers.SafeInjectTextParams{
									Bot:              bs.Bot,
									PendingWait:      bs.PendingWait,
									InjectQueue:      bs.InjectQueue,
									InjectConfirm:    bs.InjectConfirm,
									StopCooldown:     bs.StopCooldown,
									ReactionTracker:  bs.ReactionTracker,
									SessionState:     bs.SessionState,
									HookSessionLocks: &bs.HookSessionLocks,
									SessionEvents:    bs.SessionEvents,
									NotifOpQueue:     bs.NotifOpQueue,
									ResolveChat: func(t string) (*tele.Chat, string, int) {
										return cb.ResolveChat(bs, t)
									},
									FormatPaneID: notify.FormatPaneID,
								}
								helpers.SafeInjectText(p, target, text)
								peerChat, _, peerTopicID := cb.ResolveChat(bs, target)
								if peerChat != nil {
									var sendOpts []interface{}
									if peerTopicID > 0 {
										sendOpts = append(sendOpts, &tele.SendOptions{ThreadID: peerTopicID})
									}
									targetHeader := helpers.BuildAtHeader(hookAgentName, peer) + "\n---\n" + instr + "\n---\n"
									helpers.SendPagedForward(bs.Bot, peerChat, targetHeader, cnt, bs.Pages, "", sendOpts...)
								}
							}(peerInfo.TmuxTarget, msg, instructions, content, peerName)
						}
					}
					bs.SessionWatch.Notify(hookAgentName, stores.WatchEvent{
						Event:   "Stop",
						Agent:   hookAgentName,
						Summary: "Task completed",
						Detail:  helpers.TruncateStr(body, 500),
					})
					bs.CompactTools.Reset(p.SessionID)
					// Flush queued inject items after Stop via async dispatch to avoid blocking hook response
					if p.TmuxTarget != "" {
						bs.SessionEvents.DispatchAsync(p.SessionID, "flush:stop", func() error {
							cb.FlushInjectQueue(bs, p.TmuxTarget)
							return nil
						})
					}
					// Check for CC version update after session becomes idle
					if p.TmuxTarget != "" {
						go cb.CheckSessionVersion(bs, p.TmuxTarget)
					}
				}
			case "PreToolUse":
				if p.TmuxTarget != "" {
					bs.HookRunning.SetRunning(p.TmuxTarget)
					cb.TypingLog("state: event=PreToolUse target=%s state=running", p.TmuxTarget)
				}
				CancelPendingWaitBySession(bs, p.SessionID)
				// Skip TG notifications for subagent tool calls
				if p.AgentID != "" {
					break
				}
				if p.TmuxTarget != "" {
					bs.ReactionTracker.PromotePending(bs.Bot, p.TmuxTarget)
				}
				// Codex uses transcript-based Update; CC uses streaming boundary flush
				var body string
				if p.Backend == "codex" {
					if chat != nil && p.ToolName != "AskUserQuestion" {
						body = cb.ProcessTranscriptUpdates(bs, p.SessionID, p.TranscriptPath)
						if body != "" {
							cb.SendEventNotification(bs, chat, chatID, p.SessionID, "PreToolUse", p.Project, cwdForRoute, p.TmuxTarget, body, "", hookAgentName, hookTopicID)
							// Accumulate Update text for @ channel forwarding
							if hookAgentName != "" {
								bs.AtChannels.AppendBuffer(hookAgentName, body)
							}
						} else {
							logger.Info(fmt.Sprintf("PreToolUse Update skipped: session=%s tool=%s reason=no_new_assistant_text", p.SessionID, p.ToolName))
						}
					}
					// Reset compact tool message cycle when new assistant text arrives (independent of whitelist)
					if body != "" {
						bs.CompactTools.Reset(p.SessionID)
					}
				} else {
					if p.ToolName != "AskUserQuestion" {
						cb.StreamFlush(bs, p.SessionID, false)
					}
				}
				// Send tool detail notification if configured
				toolCfg, _ := config.LoadAppConfig()
				if p.ToolName != "AskUserQuestion" && helpers.ShouldNotifyTool(p.ToolName, toolCfg.ToolNotifyEnabled, toolCfg.ToolNotifyList) {
					toolChat, toolChatID, toolTopicID := chat, chatID, hookTopicID
					if chat != nil && helpers.IsRouteToolNotifyOff(bs.SessionState, p.TmuxTarget) {
						toolChat, toolChatID, toolTopicID = helpers.GetPrivateChat()
					}
					if toolChat != nil && toolCfg.ToolNotifyCompact {
						// Compact mode: accumulate tool lines in a single message
						compactLine := notify.BuildCompactToolLine(p.ToolName, p.ToolInput, cwdForRoute, toolCfg.CompactToolMaxLen)
						existing, exists := bs.CompactTools.Get(p.SessionID)
						if exists {
							existing.Lines = append(existing.Lines, compactLine)
							compactBody := strings.Join(existing.Lines, "\n")
							nd := notify.NotificationData{
								Event: "CompactTool", Project: p.Project, CWD: cwdForRoute,
								TmuxTarget: p.TmuxTarget, AgentName: hookAgentName,
								Backend: func() string {
									if info := bs.SessionState.FindInfoByTarget(p.TmuxTarget); info != nil && info.Backend != "" {
										return info.Backend
									}
									return "cc"
								}(),
								CLICommand:     helpers.GetPaneCLICommand(p.TmuxTarget),
								ContextUsedPct: -1,
								Body:           compactBody,
							}
							if usedPct, usedTokens, windowSize, ctxOk := helpers.ReadContextUsage(p.SessionID); ctxOk {
								nd.ContextUsedPct = usedPct
								nd.ContextUsedTokens = usedTokens
								nd.ContextWindowSize = windowSize
							}
							fullText := notify.BuildNotificationText(nd)
							if len([]rune(fullText)) > 4000 {
								// Overflow: reset and send as new message
								bs.CompactTools.Reset(p.SessionID)
								nd.Body = compactLine
								overflowText := notify.BuildNotificationText(nd)
								var sendOpts []interface{}
								sendOpts = append(sendOpts, tele.ModeHTML)
								if existing.TopicID > 0 {
									sendOpts = append(sendOpts, &tele.SendOptions{ThreadID: existing.TopicID})
								}
								sent, err := helpers.RetrySend(bs.Bot, &tele.Chat{ID: existing.ChatID}, overflowText, sendOpts...)
								if err != nil {
									logger.Error(fmt.Sprintf("compact tool overflow send failed: %v", err))
								} else if sent != nil {
									bs.CompactTools.Store(p.SessionID, &stores.CompactToolEntry{
										MsgID: sent.ID, ChatID: existing.ChatID, TopicID: existing.TopicID,
										Lines: []string{compactLine},
									})
									logger.Debug(fmt.Sprintf("compact tool overflow sent: session=%s msg_id=%d tool=%s\n<<<BODY\n%s\nBODY>>>", p.SessionID, sent.ID, p.ToolName, overflowText))
								}
							} else {
								editMsg := &tele.Message{ID: existing.MsgID, Chat: &tele.Chat{ID: existing.ChatID}}
								helpers.RetryEdit(bs.Bot, editMsg, fullText, tele.ModeHTML)
								logger.Debug(fmt.Sprintf("compact tool edited: session=%s msg_id=%d tool=%s lines=%d\n<<<BODY\n%s\nBODY>>>", p.SessionID, existing.MsgID, p.ToolName, len(existing.Lines), fullText))
							}
						} else {
							compactBody := compactLine
							nd := notify.NotificationData{
								Event: "CompactTool", Project: p.Project, CWD: cwdForRoute,
								TmuxTarget: p.TmuxTarget, AgentName: hookAgentName,
								Backend: func() string {
									if info := bs.SessionState.FindInfoByTarget(p.TmuxTarget); info != nil && info.Backend != "" {
										return info.Backend
									}
									return "cc"
								}(),
								CLICommand:     helpers.GetPaneCLICommand(p.TmuxTarget),
								ContextUsedPct: -1,
								Body:           compactBody,
							}
							if usedPct, usedTokens, windowSize, ctxOk := helpers.ReadContextUsage(p.SessionID); ctxOk {
								nd.ContextUsedPct = usedPct
								nd.ContextUsedTokens = usedTokens
								nd.ContextWindowSize = windowSize
							}
							fullText := notify.BuildNotificationText(nd)
							var sendOpts []interface{}
							sendOpts = append(sendOpts, tele.ModeHTML)
							if toolTopicID > 0 {
								sendOpts = append(sendOpts, &tele.SendOptions{ThreadID: toolTopicID})
							}
							sent, err := helpers.RetrySend(bs.Bot, toolChat, fullText, sendOpts...)
							if err != nil {
								logger.Error(fmt.Sprintf("compact tool send failed: %v", err))
							} else if sent != nil {
								bs.CompactTools.Store(p.SessionID, &stores.CompactToolEntry{
									MsgID: sent.ID, ChatID: toolChat.ID, TopicID: toolTopicID,
									Lines: []string{compactLine},
								})
								logger.Debug(fmt.Sprintf("compact tool sent: session=%s msg_id=%d tool=%s chat=%s\n<<<BODY\n%s\nBODY>>>", p.SessionID, sent.ID, p.ToolName, toolChatID, fullText))
							}
						}
					} else if toolChat != nil {
						// Standard detailed mode
						toolText := notify.BuildToolNotifyText(p.ToolName, p.ToolInput, cwdForRoute)
						if toolText != "" {
							msgID := cb.SendEventNotification(bs, toolChat, toolChatID, p.SessionID, "ToolUse", p.Project, cwdForRoute, p.TmuxTarget, toolText, p.ToolName, hookAgentName, toolTopicID)
							if p.ToolUseID != "" && msgID > 0 {
								bs.ToolUseMsgs.Store(p.ToolUseID, &stores.ToolUseMsgEntry{MsgID: msgID, ChatID: toolChat.ID, TopicID: toolTopicID, Body: toolText})
							}
						}
					}
				}
			case "PostToolUse", "PostToolUseFailure":
				// Correlate with pending wait entry and freeze TG button via ResolveIfUnresolved
				if waitEntry, ok := bs.PendingWait.FindMatch(p.SessionID, p.ToolName, helpers.CanonicalToolInput(p.ToolInput), p.ToolUseID); ok {
					label := "✅ Allowed on desktop"
					if waitEntry.ToolName == "AskUserQuestion" {
						var resp struct {
							Answers map[string]string `json:"answers"`
						}
						json.Unmarshal(p.ToolResponse, &resp)
						label = "⌨️ " + helpers.FormatAnswers(resp.Answers)
					}
					// FreezeWaitEntryOnDesktop uses ResolveIfUnresolved + TryEnqueue EDIT
					if snap, ok := bs.PendingWait.GetSnapshot(waitEntry.UUID); ok {
						helpers.FreezeWaitEntryOnDesktop(bs.Bot, bs.PendingWait, bs.NotifOpQueue, snap, label)
					}
					logger.Info(fmt.Sprintf("Resolved on desktop: uuid=%s tool=%s tool_use_id=%q label=%s", waitEntry.UUID, waitEntry.ToolName, p.ToolUseID, label))
				}
				if event == "PostToolUseFailure" {
					break
				}
				if p.ToolUseID == "" {
					break
				}
				if p.ToolName == "AskUserQuestion" && p.TmuxTarget != "" {
					var resp struct {
						Answers map[string]string `json:"answers"`
					}
					json.Unmarshal(p.ToolResponse, &resp)
					bs.InjectConfirm.NotifyAskAnswered(p.TmuxTarget, resp.Answers)
				}
				entry, ok := bs.ToolUseMsgs.Get(p.ToolUseID)
				if !ok {
					logger.Debug(fmt.Sprintf("PostToolUse: no stored msg for tool_use_id=%s", p.ToolUseID))
					break
				}
				bs.ToolUseMsgs.Delete(p.ToolUseID)
				resultText := notify.BuildToolResultText(p.ToolName, p.ToolResponse)
				combinedBody := entry.Body + "\n\n" + resultText
				postBackend := "cc"
				if p.Backend != "" {
					postBackend = p.Backend
				}
				nd := notify.NotificationData{
					Event: "ToolUse", Project: p.Project, CWD: cwdForRoute,
					TmuxTarget: p.TmuxTarget, ToolName: p.ToolName,
					AgentName: hookAgentName, Backend: postBackend,
					CLICommand: helpers.GetPaneCLICommand(p.TmuxTarget),
					ContextUsedPct: -1,
				}
				if usedPct, usedTokens, windowSize, ctxOk := helpers.ReadContextUsage(p.SessionID); ctxOk {
					nd.ContextUsedPct = usedPct
					nd.ContextUsedTokens = usedTokens
					nd.ContextWindowSize = windowSize
				}
				postCfg, _ := config.LoadAppConfig()
				paginationMax := 4000
				if postCfg.PaginationMaxRunes > 0 {
					paginationMax = postCfg.PaginationMaxRunes
				}
				headerLen := notify.HeaderLen(nd)
				maxBodyRunes := paginationMax - headerLen - 100
				chunks := helpers.SplitBody(combinedBody, maxBodyRunes)
				editChat := &tele.Chat{ID: entry.ChatID}
				editMsg := &tele.Message{ID: entry.MsgID, Chat: editChat}
				if len(chunks) <= 1 {
					nd.Body = combinedBody
					fullText := notify.BuildNotificationText(nd)
					helpers.RetryEdit(bs.Bot, editMsg, fullText, tele.ModeHTML)
				} else {
					nd.Body = chunks[0]
					nd.Page = 1
					nd.TotalPages = len(chunks)
					fullText := notify.BuildNotificationText(nd)
					kb := helpers.BuildPageKeyboard(1, len(chunks))
					helpers.RetryEdit(bs.Bot, editMsg, fullText, kb, tele.ModeHTML)
					bs.Pages.Store(entry.MsgID, p.SessionID, &stores.PageEntry{
						Chunks: chunks, Event: "ToolUse", Project: p.Project,
						CWD: cwdForRoute, TmuxTarget: p.TmuxTarget, ChatID: entry.ChatID,
						CLICommand:        nd.CLICommand,
						AgentName:         nd.AgentName,
						Backend:           nd.Backend,
						ContextUsedPct:    nd.ContextUsedPct,
						ContextUsedTokens: nd.ContextUsedTokens,
						ContextWindowSize: nd.ContextWindowSize,
					})
				}
				logger.Info(fmt.Sprintf("PostToolUse: updated msg_id=%d tool=%s tool_use_id=%s result_len=%d", entry.MsgID, p.ToolName, p.ToolUseID, len(resultText)))
				// Flush inject queue after tool execution via async dispatch to avoid blocking hook response
				if p.ToolName != "AskUserQuestion" && p.TmuxTarget != "" {
					if bs.InjectQueue.HasItems(p.TmuxTarget) {
						logger.Info(fmt.Sprintf("PostToolUse: flushing inject queue for target=%s", p.TmuxTarget))
						bs.SessionEvents.DispatchAsync(p.SessionID, "flush:postToolUse", func() error {
							cb.FlushInjectQueue(bs, p.TmuxTarget)
							return nil
						})
					}
				}
			case "PermissionRequest":
				// PermissionRequest is now handled via file-based communication
				// hook.go writes pending file and polls for answer
				// This handler is no longer used by hook.go, but kept for backward compatibility
				logger.Info(fmt.Sprintf("PermissionRequest received via HTTP (legacy path): tool=%s", p.ToolName))
				return nil
			default:
				// Unknown event — send notification if possible
				if chat != nil {
					body := cb.ProcessTranscriptUpdates(bs, p.SessionID, p.TranscriptPath)
					cb.SendEventNotification(bs, chat, chatID, p.SessionID, event, p.Project, cwdForRoute, p.TmuxTarget, body, "", hookAgentName, hookTopicID)
				}
			}
			return nil
		})
		w.WriteHeader(200)
	})
}

// handleMessageDisplay handles the MessageDisplay fast-path: appends delta to the stream store and returns
// immediately — no Telegram I/O inline. The background worker and boundary flushes handle the I/O.
func handleMessageDisplay(bs *types.BotState, cb Callbacks, p *helpers.HookPayload) {
	if p.SessionID == "" || p.MessageID == "" {
		return // final-with-empty-delta IS still recorded
	}
	logDelta := func() {
		logger.Info(fmt.Sprintf("MessageDisplay delta: session=%s message_id=%s turn_id=%s index=%d final=%v delta=%s",
			p.SessionID, p.MessageID, p.TurnID, p.Index, p.Final, helpers.TruncateStr(p.Delta, 200)))
	}
	// Hot path: existing message_id → append without resolving chat (no LoadCredentials per delta)
	if handled, dropped := bs.Streams.AppendExisting(p.SessionID, p.MessageID, p.TurnID, p.Index, p.Delta, p.Final); handled {
		if dropped {
			logger.Info(fmt.Sprintf("MessageDisplay delta dropped: session=%s message_id=%s turn_id=%s index=%d reason=closed_stopped_or_sealed",
				p.SessionID, p.MessageID, p.TurnID, p.Index))
		} else {
			logDelta()
		}
		return
	}
	// First delta of a new message_id: register session only if unknown (SessionState.Add writes sessions.json —
	// keep disk I/O off the per-delta path) + resolve chat once.
	if p.TmuxTarget != "" && bs.SessionState.FindInfoByTarget(p.TmuxTarget) == nil {
		bs.SessionState.Add(p.SessionID, p.TmuxTarget, p.CWD, p.TranscriptPath)
	}
	chat, _, topicID := cb.ResolveChat(bs, p.TmuxTarget)
	if chat == nil {
		return
	}
	info := bs.SessionState.FindInfoByTarget(p.TmuxTarget)
	agentName, cwd, backend := "", p.CWD, "cc"
	if info != nil {
		agentName = info.Name
		if info.CWD != "" {
			cwd = info.CWD
		}
		if info.Backend != "" {
			backend = info.Backend
		}
	}
	if bs.Streams.AppendDelta(p.SessionID, stores.StreamMeta{
		MessageID:  p.MessageID,
		TurnID:     p.TurnID,
		TmuxTarget: p.TmuxTarget,
		Project:    p.Project,
		CWD:        cwd,
		AgentName:  agentName,
		Backend:    backend,
		ChatID:     chat.ID,
		TopicID:    topicID,
	}, p.Index, p.Delta, p.Final) {
		bs.CompactTools.Reset(p.SessionID) // new assistant text resets compact-tool cycle
	}
	logDelta()
}
