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

// budget and b2Floor restore the boss-approved 56e071f tool-boundary values that were silently replaced by a
// worker-chosen boundaryGrace=250ms during the dual-FIFO rework (boss f23 restoration order). budget is the
// B3 timeout — the absolute-deadline ceiling (added to the PreToolUse ingress arrivedAt) for a lagging pre-tool
// MessageDisplay: eligible MD arriving at or before ptu.arrivedAt + budget are drained/waited-for before the
// tool notification so their text renders first. b2Floor is the early-exit floor — a queued-boundary
// (fast-tool / next-tool) resolution must NOT resolve the S6 wait before ptu.arrivedAt + b2Floor, so a lagging
// pre-tool MD still has until the floor to land and win (drain); an eligible pre-tool MD wins at ANY time,
// including inside the floor.
const budget = 1500 * time.Millisecond
const b2Floor = 500 * time.Millisecond

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
		// Normalise pi's lowercase tool names onto the canonical CC names ONCE at ingest, before p.ToolName's
		// first use, so all downstream CC logic (ShouldNotifyTool/knownTools, BuildCompactToolLine, the Tools
		// menu, PendingWait match) applies unchanged. The archived `raw` payload keeps pi's actual name. `ls`
		// stays lowercase (no CC analogue) so it keeps falling through to the "Other" bucket.
		if p.Backend == "pi" {
			p.ToolName = helpers.NormalizePiToolName(p.ToolName)
		}
		// Capture the ingress arrival time IMMEDIATELY after parse + FormatPaneID. This is the authoritative
		// best-effort arrival stamp — carried into the queued job metadata (drain eligibility deadline) and
		// stamped onto the archive ts (RecordHookEventAt) so archive ORDER BY ts == arrival order even though
		// event_id reflects drain-perturbed handler-execution order.
		arrivedAt := time.Now()
		// ENQUEUE IMMEDIATELY (S0 ordering reservation, rev 14 BLOCKER1): the SYNC metadata Dispatch onto the
		// Hook FIFO is the per-session ordering reservation, done BEFORE any archive I/O — a slow SQLite write
		// cannot reorder the reservation. ALL hooks (incl. MessageDisplay) are now serialized through this FIFO;
		// the archive write, the MessageDisplay branch, re-register, prelude, and switch all run INSIDE the job.
		bs.SessionEvents.DispatchWithMeta(p.SessionID, "hook:"+event, event, p.PromptID, arrivedAt, func() error {
			// Archive every hook event (Change 2) at the TOP of the queued handler, stamping the ingress
			// arrivedAt at ns precision (RecordHookEventAt). Degraded-mode: log + continue, never block the hook.
			if helpers.Archive != nil {
				if aerr := helpers.Archive.RecordHookEventAt(event, p.SessionID, p.MessageID, p.TurnID, p.PromptID, raw, arrivedAt); aerr != nil {
					logger.Warn("message archive: RecordHookEventAt failed: " + aerr.Error())
				}
			}
			// MessageDisplay is now processed ON the Hook FIFO (S0) — no TG I/O, appends the delta and returns.
			if event == "MessageDisplay" {
				handleMessageDisplay(bs, cb, p, raw)
				return nil
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
					// §F #1: the SessionEnd MAIN notification is a blocking send reachable on the Hook FIFO —
					// route it onto the Message FIFO. The msg_id is not reused, so DispatchAsync (fire-and-forget,
					// enqueued in Hook-FIFO order).
					endChat := chat
					endChatID := chatID
					endText := text
					endProject := p.Project
					endTarget := p.TmuxTarget
					bs.MessageQueue.DispatchAsync(p.SessionID, "msg:sessionend-main", func() error {
						helpers.RetrySend(bs.Bot, endChat, endText, sessionEndOpts...)
						logger.Info(fmt.Sprintf("Notification sent to chat %s: SessionEnd [%s] tmux=%s", endChatID, endProject, endTarget))
						return nil
					})
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
							// §F #2 STATE/IO SPLIT: capture peerChat+msg+opts on the Hook FIFO (state), enqueue the
							// blocking peer-notify send onto the Message FIFO (DispatchAsync — the msg_id is not reused).
							peerSendChat := peerChat
							peerSendMsg := msg
							peerSendOpts := sendOpts
							bs.MessageQueue.DispatchAsync(p.SessionID, "msg:sessionend-peer", func() error {
								helpers.RetrySend(bs.Bot, peerSendChat, peerSendMsg, peerSendOpts...)
								return nil
							})
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
								HookRunning:      bs.HookRunning,
								HookSessionLocks: &bs.HookSessionLocks,
								SessionEvents:    bs.SessionEvents,
								PendingMsgStore:  bs.PendingMsgStore,
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
				// S3b: compact-cycle + tool-use state cleanup stays on the Hook FIFO (INV5). resetCompactAndScheduleDelete
				// discards the compact entry and enqueues a Message-FIFO msg:id-delete for its mapping; CleanupSession
				// drops any abandoned PreToolUse tool-use entry for this session.
				resetCompactAndScheduleDelete(bs, p.SessionID)
				bs.ToolUseMsgs.CleanupSession(p.SessionID)
				bs.SessionCounts.Cleanup(p.SessionID)
				CancelPendingWaitBySession(bs, p.SessionID, "", "", "") // cancel (resolve + push cancel) any still-pending hook waits for this session
				// S3b lifecycle-as-op: route EndSession (TTL tombstone; straggler deltas after SessionEnd are dropped)
				// + MsgIDMap.DeleteSession through the Message FIFO (SYNC Dispatch) so the clear is totally ordered
				// with every render op and the map delete runs after all prior mapping ops for this session (INV6).
				bs.MessageQueue.Dispatch(p.SessionID, "msg:lifecycle-endsession", func() error {
					bs.Streams.EndSession(p.SessionID)
					bs.MsgIDMap.DeleteSession(p.SessionID)
					return nil
				})
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
				CancelPendingWaitBySession(bs, p.SessionID, "", "", "")
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
				// S3b lifecycle-as-op: route Rotate through the Message FIFO (SYNC Dispatch, no lock held) so the
				// stream-clear is totally ordered with every render op — no render-after-clear race (INV6).
				bs.MessageQueue.Dispatch(p.SessionID, "msg:lifecycle-rotate", func() error {
					bs.Streams.Rotate(p.SessionID) // clear previous turn's entries for new user turn (keeps ClosedTurns tombstone)
					return nil
				})
			case "Stop":
				if p.TmuxTarget != "" {
					bs.HookRunning.SetIdle(p.TmuxTarget)
					cb.TypingLog("state: event=Stop target=%s state=idle", p.TmuxTarget)
					bs.StopCooldown.Record(p.TmuxTarget)
				}
				CancelPendingWaitBySession(bs, p.SessionID, "", "", "")
				if chat != nil {
					body := p.LastAssistantMessage
					// Auto-handle rate-limit popup
					if strings.HasPrefix(strings.TrimSpace(body), "You've hit your limit · resets") {
						logger.Info(fmt.Sprintf("Rate-limit detected in Stop body: session=%s target=%s body=%s", p.SessionID, p.TmuxTarget, body))
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
							// §F #3: route the rate-limit notification onto the Message FIFO (DispatchAsync — the
							// msg_id is not reused).
							rlChat := chat
							rlText := notifyText
							rlOpts := sendOpts
							bs.MessageQueue.DispatchAsync(p.SessionID, "msg:rate-limit", func() error {
								helpers.RetrySend(bs.Bot, rlChat, rlText, rlOpts...)
								return nil
							})
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
							// §F #4: the codex Stop notification is a blocking send reachable on the Hook FIFO —
							// route it onto the Message FIFO. The msg_id is not reused (fire-and-forget), so
							// DispatchAsync in Hook-FIFO order. Distinct from the CC-branch msg:stop-direct op below
							// (mutually exclusive backends → no double-send).
							stopChat := chat
							stopChatID := chatID
							stopBody := body
							bs.MessageQueue.DispatchAsync(p.SessionID, "msg:stop-codex", func() error {
								cb.SendEventNotification(bs, stopChat, stopChatID, p.SessionID, "Stop", p.Project, cwdForRoute, p.TmuxTarget, stopBody, "", hookAgentName, hookTopicID)
								return nil
							})
						}
					} else {
						// S10 Stop terminal state machine (no MD-waits): install the authoritative
						// last_assistant_message as the last stream entry (FinalizeLastWithText), then take a
						// terminal outcome WITHOUT any drainUntilComplete / stop-MD-grace / AwaitEntryOrStop wait.
						// The Hook FIFO holds NO stream lock while sync-Dispatching onto the Message FIFO. Every
						// branch ends in MarkStopped (guarantee).
						finalizeRes := bs.Streams.FinalizeLastWithText(p.SessionID, body)
						if finalizeRes == stores.FinalizeNoEntry && body != "" {
							directBody := body
							directChat := chat
							directChatID := chatID
							bs.MessageQueue.Dispatch(p.SessionID, "msg:stop-direct", func() error {
								cb.SendEventNotification(bs, directChat, directChatID, p.SessionID, "Stop", p.Project, cwdForRoute, p.TmuxTarget, directBody, "", hookAgentName, hookTopicID)
								return nil
							})
							bs.Streams.MarkStopped(p.SessionID)
							logger.Info(fmt.Sprintf("Stop terminal: outcome=direct_send session=%s", p.SessionID))
						} else {
							if finalizeRes == stores.FinalizeSealedMismatch && body != "" {
								// The sealed last stream entry carries an EARLIER message (a distinct post-tool text that
								// raced Stop and was dropped). Direct-send the Stop authoritative text so it is not lost;
								// the StreamFlush below still relabels the sealed entry and runs MarkStopped. The Hook FIFO
								// serializes Stop before the late dropped MD, so this cannot double-send.
								directBody := body
								directChat := chat
								directChatID := chatID
								bs.MessageQueue.Dispatch(p.SessionID, "msg:stop-direct", func() error {
									cb.SendEventNotification(bs, directChat, directChatID, p.SessionID, "Stop", p.Project, cwdForRoute, p.TmuxTarget, directBody, "", hookAgentName, hookTopicID)
									return nil
								})
								logger.Info(fmt.Sprintf("Stop terminal: outcome=direct_send_sealed_mismatch session=%s", p.SessionID))
							}
							cb.StreamFlush(bs, p.SessionID, true)
						}
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
							var historyLines []string
							for _, entry := range buffered {
								historyLines = append(historyLines, fmt.Sprintf("[%s → %s]: %s", hookAgentName, dn, entry))
							}
							triggerLine := fmt.Sprintf("[%s → %s]: %s", hookAgentName, dn, body)
							content := helpers.BuildAtForwardContent(strings.Join(historyLines, "\n"), triggerLine)
							instructions := fmt.Sprintf("`%s` completed a task. The update below has a READ-ONLY PRIOR CONTEXT block (lines prefixed `HISTORY> `, reference only) and a LIVE TRIGGER block (lines prefixed `TRIGGER> `) with the latest output.", hookAgentName)
							msg := helpers.BuildAtMsg(hookAgentName, peerName, instructions, content)
							logger.Info(fmt.Sprintf("@ forward: %s → %s body_len=%d content=%s", hookAgentName, peerName, len(content), content))
							go func(target, text, instr, cnt, peer string) {
								p := helpers.SafeInjectTextParams{
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
						Detail:  body,
					})
					// S9a: discard the compact entry (Hook FIFO) and schedule the ordered map-delete (Message FIFO).
					resetCompactAndScheduleDelete(bs, p.SessionID)
					// R10-item1: only an MD-final opens the routing window; Stop is a routing SIGNAL within an
					// already-armed window, never an independent trigger. If no window is armed, SignalEvent is a
					// no-op (returns false) and the idle ticker is the only fallback for turns with no MD-final.
					if p.TmuxTarget != "" && bs.InjectQueue.HasItems(p.TmuxTarget) {
						bs.InjectRoute.SignalEvent(p.TmuxTarget, "Stop", "")
					}
					// Check for CC version update after session becomes idle
					if p.TmuxTarget != "" {
						go cb.CheckSessionVersion(bs, p.TmuxTarget)
					}
				}
			case "agent_start":
				// pi extension POSTs agent_start at run start (before the first tool call) to mark busy.
				if p.TmuxTarget != "" {
					bs.HookRunning.SetRunning(p.TmuxTarget)
					cb.TypingLog("state: event=agent_start target=%s state=running", p.TmuxTarget)
				}
			case "agent_idle":
				// pi extension POSTs agent_idle at agent_settled when a run ended in an INTERRUPTED/ERROR
				// stopReason (aborted = ESC, error = retries exhausted) — instead of Stop (which would relabel
				// the last bubble to "Task Completed"). Clears busy (pi busy is store-held with no reaper; Stop's
				// SetIdle is the only other clear) and does NOTHING else to the turn — no
				// finalize/relabel/TryClose/StopCooldown/CancelPendingWait (pending waits are cancelled by the
				// next UserPromptSubmit, ~register.go:256). Matches CC, which posts no Stop on an interrupt.
				// R2/R3 — notifications unify UPWARD: EVERY interrupt notifies. Dispatch on stop_reason (pi's own
				// value, forwarded VERBATIM by the extension — never guessed here): "aborted" => a standalone
				// Interrupted notification; "error" => the AgentError notification carrying error_message;
				// anything else => SetIdle only, no notification.
				if p.TmuxTarget != "" {
					bs.HookRunning.SetIdle(p.TmuxTarget)
					cb.TypingLog("state: event=agent_idle target=%s state=idle", p.TmuxTarget)
				}
				if chat != nil {
					switch p.StopReason {
					case "aborted":
						cb.SendEventNotification(bs, chat, chatID, p.SessionID, "AgentInterrupted", p.Project, cwdForRoute, p.TmuxTarget, "⏹ pi run interrupted", "", hookAgentName, hookTopicID)
					case "error":
						errBody := "⚠️ pi run error\n\n" + p.ErrorMessage
						cb.SendEventNotification(bs, chat, chatID, p.SessionID, "AgentError", p.Project, cwdForRoute, p.TmuxTarget, errBody, "", hookAgentName, hookTopicID)
					}
				}
			case "agent_retry":
				// pi extension POSTs agent_retry at the retry's agent_start when the PREVIOUS run of this SAME turn
				// ended in a retryable provider error (stream truncation / overloaded / 429 / 5xx) and pi is auto-
				// continuing. Mark the already-rendered (truncated) bubble of this turn interrupted-and-retrying
				// (Item 7): a complete answer follows in a new bubble below. The retry's own agent_start already
				// SetRunning, so agent_retry touches ONLY the bubble — no busy state, no notification. Complementary
				// to (never disturbs) the agent_idle settled path (Item 3b: retries EXHAUSTED -> AgentError).
				if bs.Streams.MarkInterruptedRetry(p.SessionID, p.TurnID) {
					cb.FlushStreamOp(bs, p.SessionID) // re-render the sealed bubble once with the interrupted mark
					logger.Info(fmt.Sprintf("agent_retry: marked interrupted bubble session=%s turn_id=%s error=%s", p.SessionID, p.TurnID, p.ErrorMessage))
				} else {
					logger.Info(fmt.Sprintf("agent_retry: no bubble to mark session=%s turn_id=%s", p.SessionID, p.TurnID))
				}
			case "PreToolUse":
				if p.TmuxTarget != "" {
					bs.HookRunning.SetRunning(p.TmuxTarget)
					cb.TypingLog("state: event=PreToolUse target=%s state=running", p.TmuxTarget)
				}
				// R10-item1: only an MD-final opens the routing window; PreToolUse is a routing SIGNAL within an
				// already-armed window, never an independent trigger. If no window is armed, SignalEvent is a
				// no-op (returns false) and the idle ticker is the only fallback for turns with no MD-final.
				if p.TmuxTarget != "" && bs.InjectQueue.HasItems(p.TmuxTarget) {
					bs.InjectRoute.SignalEvent(p.TmuxTarget, "PreToolUse", p.ToolName)
				}
				// Round 8: skip THIS tool's own pending entry (async can register its PermissionRequest before
				// this PreToolUse runs on the FIFO) — cancel only stale entries from earlier tools.
				CancelPendingWaitBySession(bs, p.SessionID, p.ToolName, helpers.CanonicalToolInput(p.ToolInput), p.ToolUseID)
				// Skip TG notifications for subagent tool calls
				if p.AgentID != "" {
					break
				}
				if p.TmuxTarget != "" {
					// §F #5 (B.7 STATE/IO SPLIT): TakeReactions removes the tracked entries on the Hook FIFO
					// (state); the clear I/O (ApplyClear) is enqueued onto the Message FIFO (DispatchAsync).
					clearEntries := bs.ReactionTracker.TakeReactions(p.TmuxTarget)
					if len(clearEntries) > 0 {
						bs.MessageQueue.DispatchAsync(p.SessionID, "msg:clear-reactions", func() error {
							bs.ReactionTracker.ApplyClear(bs.Bot, clearEntries)
							return nil
						})
					}
				}
				// Codex uses transcript-based Update; CC uses streaming boundary flush
				var body string
				if p.Backend == "codex" {
					if chat != nil && p.ToolName != "AskUserQuestion" {
						body = cb.ProcessTranscriptUpdates(bs, p.SessionID, p.TranscriptPath)
						if body != "" {
							// §F #6: the codex PreToolUse Update is a blocking send reachable on the Hook FIFO —
							// route it onto the Message FIFO. DispatchAsync (enqueued in Hook-FIFO order so codex
							// updates stay ordered; the msg_id is not reused).
							updChat := chat
							updChatID := chatID
							updBody := body
							bs.MessageQueue.DispatchAsync(p.SessionID, "msg:codex-update", func() error {
								cb.SendEventNotification(bs, updChat, updChatID, p.SessionID, "PreToolUse", p.Project, cwdForRoute, p.TmuxTarget, updBody, "", hookAgentName, hookTopicID)
								return nil
							})
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
						// S9a: discard the compact entry (Hook FIFO) + schedule the ordered map-delete (Message FIFO).
						resetCompactAndScheduleDelete(bs, p.SessionID)
					}
				} else {
					if p.ToolName != "AskUserQuestion" {
						// S6 QUEUE-LOOKAHEAD (rev 13 predicate; rev 14/15/16 ordering fixes): drain/wait for any
						// LAGGING pre-tool MessageDisplay for THIS tool boundary on the Hook FIFO, then flush it
						// before the tool notification (text -> tool). Replaces the old poll-external-state
						// tool-boundary wait. arrivedAt is the in-scope ingress stamp of this PTU job. f23 (boss
						// restoration): deadline = arrivedAt + budget (1500ms), floor = arrivedAt + b2Floor (500ms).
						deadline := arrivedAt.Add(budget)
						floor := arrivedAt.Add(b2Floor)
						// eligible: a MessageDisplay that arrived within the absolute deadline and is prompt-
						// compatible (a non-empty prompt_id mismatch disqualifies; empty on either side is fine).
						eligible := func(m stores.JobMeta) bool {
							return m.Event == "MessageDisplay" && !m.ArrivedAt.After(deadline) &&
								!(m.PromptID != "" && p.PromptID != "" && m.PromptID != p.PromptID)
						}
						// boundary: the front-scan STOPS here — the next tool, THIS tool's own PostToolUse (so a
						// post-tool MD is never drained into the pre-tool flush), a turn-terminal, anything past the
						// deadline, or a different-prompt MessageDisplay (rev 15 MAJOR1 zero-grace terminate).
						boundary := func(m stores.JobMeta) bool {
							return m.Event == "PreToolUse" || m.Event == "PostToolUse" || m.Event == "Stop" || m.Event == "UserPromptSubmit" ||
								m.Event == "SessionEnd" || m.ArrivedAt.After(deadline) ||
								(m.Event == "MessageDisplay" && m.PromptID != "" && p.PromptID != "" && m.PromptID != p.PromptID)
						}
						// (a) snapshot+clear the marker BEFORE the drain (a drain may re-set it for bubble B).
						initialText := bs.Streams.TakeNewTextSinceTool(p.SessionID)
						// (b) ALWAYS run every already-queued eligible pre-tool MD now (incl. a 2nd bubble), front-
						// scanning and stopping at a boundary (rev 15 BLOCKER — the drain is never skipped).
						n := bs.SessionEvents.DrainAndRunMatching(p.SessionID, eligible, boundary)
						// (c) WAIT only when no pre-tool text has arrived AND nothing was queued. f23: the floored
						// variant honors the b2Floor=500ms early-exit floor and the budget=1500ms deadline (nil
						// resolve = the pre-f23 drain-until-deadline semantics; an eligible pre-tool MD still drains
						// and thus renders before the tool at ANY time, including inside the floor).
						waitBranch := "skipped"
						if !initialText && n == 0 {
							waitBranch = bs.SessionEvents.WaitForMatchOrDeadlineFloored(p.SessionID, p.PromptID, eligible, boundary, nil, floor, deadline)
						}
						// (d) consume the post-drain marker on its OWN statement (NO || short-circuit, rev 16
						// BLOCKER3) so a drain-set marker (bubble B) cannot leak to the next PTU, then merge.
						drainedText := bs.Streams.TakeNewTextSinceTool(p.SessionID)
						hadNewText := initialText || drainedText
						// f23: log the tool-boundary wait resolution (branch + elapsed since ingress) like the
						// original 56e071f streamFlushAwaitToolBoundary did, so raws prove the restored window fired.
						logger.Info(fmt.Sprintf("tool-boundary wait done: session=%s prompt=%s branch=%s had_text=%v n_drained=%d elapsed_ms=%d floor_ms=%d budget_ms=%d",
							p.SessionID, p.PromptID, waitBranch, hadNewText, n, time.Since(arrivedAt).Milliseconds(), b2Floor.Milliseconds(), budget.Milliseconds()))
						// (e) UNCONDITIONAL plain pre-tool flush via the NON-DRAINING op (no drainUntilComplete —
						// the lookahead above already drained/waited). Renders the pre-tool text before the notify;
						// a cheap no-op when nothing is renderable (S3).
						cb.FlushStreamOp(bs, p.SessionID)
						// (f) f25: reset the compact-tool cycle iff a NEW TG text message was (or will be) placed
						// BELOW the last tool notification since the last tool — the placement signal, NOT any-text.
						// A continuation that only EDITS the bubble above the tool must not split the compact group
						// (rev 14 BLOCKER3 false split). SendBelowSinceTool is set in flushSession p3 at enqueue time,
						// incl. the pre-tool flush at (e) above, so TAKE it AFTER cb.FlushStreamOp. (No-op in standard
						// mode — ResetAndTakeInternalID returns 0. Serialized with the Get/Store below on the Hook FIFO
						// worker — the off-FIFO Reset-vs-Store race stays fixed.)
						sendBelow := bs.Streams.TakeSendBelowSinceTool(p.SessionID)
						if sendBelow {
							resetCompactAndScheduleDelete(bs, p.SessionID)
						}
						// f25 observability (codexnote: not behavior proof): distinguish the any-text union (had_text,
						// wait-skip) from the placement signal (new_send_below, reset) in the raws.
						logger.Info(fmt.Sprintf("compact reset decision: session=%s prompt=%s new_send_below=%v had_text=%v",
							p.SessionID, p.PromptID, sendBelow, hadNewText))
					}
				}
				// Send tool detail notification if configured
				toolCfg, _ := config.LoadAppConfig()
				if p.ToolName == "AskUserQuestion" {
					// AskUserQuestion has its own PendingWait/permission notification flow — no tool-detail
					// notification is emitted here. (Fix 13c: skip is intentional, logged at Debug.)
					logger.Debug(fmt.Sprintf("ToolUse: no tool-detail notification for AskUserQuestion (handled by PendingWait flow) tool_use_id=%s", p.ToolUseID))
				} else if !helpers.ShouldNotifyTool(p.ToolName, toolCfg.ToolNotifyEnabled, toolCfg.ToolNotifyList) {
					// Fix 13c: notification skipped because tool-notify is disabled or the tool is not in the
					// notify list. Log so future investigations are not blind about missing tool notifications.
					logger.Debug(fmt.Sprintf("ToolUse: notification disabled for tool=%s (tool-notify off or not in list)", p.ToolName))
				} else {
					// S7 shared PreToolUse point: a tool notification WILL be emitted (compact or standard),
					// so record the tool-notify mark BEFORE the branch for the late-MD residual observability.
					bs.Streams.SetLastToolNotify(p.SessionID, p.PromptID)
					toolChat, toolChatID, toolTopicID := chat, chatID, hookTopicID
					if chat != nil && helpers.IsRouteToolNotifyOff(bs.SessionState, p.TmuxTarget) {
						toolChat, toolChatID, toolTopicID = helpers.GetPrivateChat()
					}
					if toolChat == nil {
						// Fix 13c: no target chat resolved (routing off / no configured chat). Log the skip.
						logger.Debug(fmt.Sprintf("ToolUse: no target chat, skipping notification for tool=%s", p.ToolName))
					} else if toolCfg.ToolNotifyCompact {
						// Compact mode: accumulate tool lines in a single message (sent via rich Bot API)
						// Fix 14: each compact tool notification is a collapsible <details> — the <summary> is the
						// compact one-liner (<=CompactToolMaxLen/compactMaxLen runes), the collapsed body is the full
						// tool-call args, so the user can expand to see the actual command. Rich path only.
						compactLine := notify.BuildCompactToolDetails(p.ToolName, p.ToolInput, cwdForRoute, toolCfg.CompactToolMaxLen)
						// Rich budget for overflow guard (mirrors bot_helpers.go rich path threshold)
						richMax := 30000
						if toolCfg.RichMaxRunes > 0 {
							richMax = toolCfg.RichMaxRunes
						}
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
							if len([]rune(fullText)) > richMax {
								// S7 overflow: take the discarded entry's internal id + allocate a fresh id on the
								// Hook FIFO, store the fresh entry, then enqueue ONE Message-FIFO op that deletes the
								// old mapping (ordered after all prior oldID ops), sends the new rich message, and Sets
								// the new mapping. All map mutations + I/O on the Message FIFO (INV4/INV5).
								oldID := bs.CompactTools.ResetAndTakeInternalID(p.SessionID)
								newID := bs.MsgIDMap.Allocate()
								overflowChatID := existing.ChatID
								overflowTopicID := existing.TopicID
								bs.CompactTools.Store(p.SessionID, &stores.CompactToolEntry{
									InternalID: newID, ChatID: overflowChatID, TopicID: overflowTopicID,
									Lines: []string{compactLine}, Rich: true,
								})
								nd.Body = compactLine
								overflowText := notify.BuildNotificationText(nd)
								session := p.SessionID
								toolName := p.ToolName
								bs.MessageQueue.DispatchAsync(session, "msg:compact-overflow", func() error {
									if oldID != 0 {
										bs.MsgIDMap.Delete(oldID)
									}
									sent, err := helpers.RetrySendRich(bs.Bot, &tele.Chat{ID: overflowChatID}, overflowText, helpers.RichSendOpts{
										TopicID:             overflowTopicID,
										SkipEntityDetection: true,
										LegacyHTML:          overflowText,
									})
									if err != nil {
										logger.Error(fmt.Sprintf("compact tool overflow send failed: %v", err))
										return nil
									}
									if sent != nil {
										bs.MsgIDMap.Set(newID, sent.ID, session)
										logger.Debug(fmt.Sprintf("compact tool overflow sent: session=%s msg_id=%d tool=%s fmt=rich\n<<<BODY\n%s\nBODY>>>", session, sent.ID, toolName, helpers.FinalizeRichHTML(overflowText)))
									}
									return nil
								})
							} else {
								// S7 edit: enqueue a Message-FIFO op carrying the entry's internal id — it resolves the
								// TG msg id via MsgIDMap.Get and edits; a missing mapping logs + skips (NO recovery send).
								internalID := existing.InternalID
								editChatID := existing.ChatID
								session := p.SessionID
								toolName := p.ToolName
								lineCount := len(existing.Lines)
								bs.MessageQueue.DispatchAsync(session, "msg:compact-edit", func() error {
									tgMsgID, ok := bs.MsgIDMap.Get(internalID)
									if !ok {
										logger.Info(fmt.Sprintf("compact tool edit skipped: no mapping session=%s internal_id=%d tool=%s", session, internalID, toolName))
										return nil
									}
									editMsg := &tele.Message{ID: tgMsgID, Chat: &tele.Chat{ID: editChatID}}
									helpers.RetryEditRich(bs.Bot, editMsg, fullText, helpers.RichSendOpts{
										SkipEntityDetection: true,
										LegacyHTML:          fullText,
									})
									logger.Debug(fmt.Sprintf("compact tool edited: session=%s msg_id=%d tool=%s lines=%d fmt=rich\n<<<BODY\n%s\nBODY>>>", session, tgMsgID, toolName, lineCount, helpers.FinalizeRichHTML(fullText)))
									return nil
								})
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
							// S7 not-exists: allocate the internal id on the Hook FIFO, store the entry, then enqueue a
							// Message-FIFO op that sends the rich message and Sets the internal-id -> TG-msg-id mapping.
							internalID := bs.MsgIDMap.Allocate()
							bs.CompactTools.Store(p.SessionID, &stores.CompactToolEntry{
								InternalID: internalID, ChatID: toolChat.ID, TopicID: toolTopicID,
								Lines: []string{compactLine}, Rich: true,
							})
							session := p.SessionID
							toolName := p.ToolName
							sendChat := toolChat
							sendTopicID := toolTopicID
							sendChatID := toolChatID
							bs.MessageQueue.DispatchAsync(session, "msg:compact-send", func() error {
								sent, err := helpers.RetrySendRich(bs.Bot, sendChat, fullText, helpers.RichSendOpts{
									TopicID:             sendTopicID,
									SkipEntityDetection: true,
									LegacyHTML:          fullText,
								})
								if err != nil {
									logger.Error(fmt.Sprintf("compact tool send failed: %v", err))
									return nil
								}
								if sent != nil {
									bs.MsgIDMap.Set(internalID, sent.ID, session)
									logger.Debug(fmt.Sprintf("compact tool sent: session=%s msg_id=%d tool=%s chat=%s fmt=rich\n<<<BODY\n%s\nBODY>>>", session, sent.ID, toolName, sendChatID, helpers.FinalizeRichHTML(fullText)))
								}
								return nil
							})
						}
					} else {
						// Standard detailed mode
						toolText := notify.BuildToolNotifyText(p.ToolName, p.ToolInput, cwdForRoute)
						if toolText != "" {
							if p.ToolUseID != "" {
								// S8: allocate the internal id on the Hook FIFO, store the tool-use entry (with
								// SessionID for CleanupSession), then enqueue a Message-FIFO msg:tool-send op that
								// sends the notification and Sets the internal-id -> TG-msg-id mapping on success.
								internalID := bs.MsgIDMap.Allocate()
								bs.ToolUseMsgs.Store(p.ToolUseID, &stores.ToolUseMsgEntry{InternalID: internalID, ChatID: toolChat.ID, TopicID: toolTopicID, Body: toolText, Rich: true, SessionID: p.SessionID})
								session := p.SessionID
								toolName := p.ToolName
								sendChat := toolChat
								sendChatID := toolChatID
								sendTopicID := toolTopicID
								bs.MessageQueue.DispatchAsync(session, "msg:tool-send", func() error {
									msgID := cb.SendEventNotification(bs, sendChat, sendChatID, session, "ToolUse", p.Project, cwdForRoute, p.TmuxTarget, toolText, toolName, hookAgentName, sendTopicID)
									if msgID > 0 {
										bs.MsgIDMap.Set(internalID, msgID, session)
									}
									return nil
								})
								// S9 fast-tool path: if PostToolUse already arrived (fast tool beat this PreToolUse
								// under async), capture the entry's internal id, Consume the buffered signal, Delete the
								// tool-use entry, and enqueue the SAME msg:tool-result op — exactly ONE result edit per
								// tool_use_id. The result op runs AFTER the tool-send op on the Message FIFO, so the
								// mapping is Set before the result op Gets it.
								if sig, pok := bs.Streams.GetPostToolArrived(p.SessionID, p.ToolUseID); pok {
									if entry, eok := bs.ToolUseMsgs.Get(p.ToolUseID); eok {
										bs.Streams.ConsumePostToolArrived(p.SessionID, p.ToolUseID)
										bs.ToolUseMsgs.Delete(p.ToolUseID)
										enqueueToolResultEdit(bs, p.SessionID, p.Project, cwdForRoute, p.TmuxTarget, p.ToolName, hookAgentName, p.Backend, p.ToolUseID, entry, sig.Response, true)
									}
								}
							}
						} else {
							// Fix 13c: empty notification body — only reachable when tool_input is absent or
							// unparseable (BuildToolNotifyText returns a name skeleton for no-arg tools). Log so
							// a missing tool notification is never silent.
							logger.Info(fmt.Sprintf("ToolUse: empty notification body for tool=%s (unparseable/absent tool_input), skipping", p.ToolName))
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
					// FreezeWaitEntryOnDesktop: ResolveIfUnresolved + BuildFrozenMarkup (state, Hook FIFO); the
					// EditOrDefer callback enqueues msg:freeze-edit onto the Message FIFO (B.8)
					if snap, ok := bs.PendingWait.GetSnapshot(waitEntry.UUID); ok {
						helpers.FreezeWaitEntryOnDesktop(bs.Bot, bs.MessageQueue, bs.PendingWait, bs.PendingMsgStore, snap, label)
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
				if entry, ok := bs.ToolUseMsgs.Get(p.ToolUseID); ok {
					// S9: a tool-use entry exists — capture its internal id, delete the entry (Hook FIFO), and
					// enqueue the msg:tool-result op that resolves the TG msg id via MsgIDMap.Get, applies the
					// result edit, then Delete(internalID) in-op (ordered after tool-send). NO MsgID==0 sentinel.
					bs.Streams.ConsumePostToolArrived(p.SessionID, p.ToolUseID) // idempotent if the fast-tool path already consumed it
					bs.ToolUseMsgs.Delete(p.ToolUseID)
					enqueueToolResultEdit(bs, p.SessionID, p.Project, cwdForRoute, p.TmuxTarget, p.ToolName, hookAgentName, p.Backend, p.ToolUseID, entry, p.ToolResponse, false)
				} else {
					// Round 8 (countable Info): under async, PostToolUse can beat PreToolUse — the PreToolUse
					// fast-tool path handles the result edit, so a miss here is expected, not an error. Buffer the
					// arrival ON the Hook FIFO (S9 intent) so a later PreToolUse can apply the result; the ingress
					// off-FIFO SetPostToolArrived was removed by S0 (enqueue-first reservation).
					bs.Streams.SetPostToolArrived(p.SessionID, p.ToolUseID, p.ToolName, p.ToolResponse)
					logger.Info(fmt.Sprintf("PostToolUse: no stored msg (PreToolUse not yet stored / applied inline) tool_use_id=%s", p.ToolUseID))
				}
				// R9-item3: the PostToolUse-triggered inject-queue flush is REMOVED. The event-driven
				// MD-final trigger (handleMessageDisplay) + the PreToolUse/Stop supplement checks now route
				// queued injects; PostToolUse no longer flushes (the time-based flush was the AskQ-picker bug).
			case "PermissionRequest":
				// PermissionRequest is now handled via file-based communication
				// hook.go writes pending file and polls for answer
				// This handler is no longer used by hook.go, but kept for backward compatibility
				logger.Info(fmt.Sprintf("PermissionRequest received via HTTP (legacy path): tool=%s", p.ToolName))
				return nil
			default:
				// Unknown event — send notification if possible.
				// §F #7: route the default/unknown-event notification onto the Message FIFO (DispatchAsync — the
				// msg_id is not reused).
				if chat != nil {
					body := cb.ProcessTranscriptUpdates(bs, p.SessionID, p.TranscriptPath)
					defChat := chat
					defChatID := chatID
					defEvent := event
					defBody := body
					bs.MessageQueue.DispatchAsync(p.SessionID, "msg:default-notify", func() error {
						cb.SendEventNotification(bs, defChat, defChatID, p.SessionID, defEvent, p.Project, cwdForRoute, p.TmuxTarget, defBody, "", hookAgentName, hookTopicID)
						return nil
					})
				}
			}
			return nil
		})
		w.WriteHeader(200)
	})
}

// resetCompactAndScheduleDelete discards the session's compact tool entry on the Hook FIFO (INV5) and, if it
// had an internal message id, enqueues a Message-FIFO msg:id-delete so the mapping delete is ordered after any
// prior ops on that id (INV4/INV5). State mutation (ResetAndTakeInternalID) stays on the Hook FIFO; only the
// map delete runs on the Message FIFO. Phase 5 (S9a) also calls this at the other CompactTools.Reset sites.
func resetCompactAndScheduleDelete(bs *types.BotState, session string) {
	oldID := bs.CompactTools.ResetAndTakeInternalID(session)
	if oldID != 0 {
		bs.MessageQueue.DispatchAsync(session, "msg:id-delete", func() error {
			bs.MsgIDMap.Delete(oldID)
			return nil
		})
	}
}

// handleMessageDisplay handles the MessageDisplay fast-path: appends delta to the stream store and returns
// immediately — no Telegram I/O inline. The background worker and boundary flushes handle the I/O.
func handleMessageDisplay(bs *types.BotState, cb Callbacks, p *helpers.HookPayload, raw []byte) {
	// Log the full raw payload for MessageDisplay too (the /hook/ handler's Info raw-payload log is skipped
	// for MessageDisplay via the early return). Debug level — MessageDisplay is per-delta/high-frequency, so
	// this is gated behind --debug to avoid Info spam in production.
	logger.Debug(fmt.Sprintf("Raw hook payload [MessageDisplay]: %s", string(raw)))
	if p.SessionID == "" || p.MessageID == "" {
		return // final-with-empty-delta IS still recorded
	}
	// Round 8 observability: if a same-prompt MessageDisplay-final lands shortly AFTER a tool
	// notification was sent (no pre-tool text was flushed first), log it as a residual inversion.
	if p.Final && p.PromptID != "" {
		if mark, ok := bs.Streams.GetLastToolNotify(p.SessionID); ok && mark.PromptID == p.PromptID {
			if d := time.Since(mark.At); d < 3*time.Second {
				logger.Info(fmt.Sprintf("late MD after tool notify: session=%s prompt=%s delay_ms=%d (residual: tool notification preceded this pre-tool text)",
					p.SessionID, p.PromptID, d.Milliseconds()))
			}
		}
	}
	// R9-item3 MD-final trigger: when this is a final delta AND the inject queue has items, open the
	// event-driven routing window (replaces the removed PostToolUse flush). This is a separate concern from
	// the post-Stop late-MD stream handling below (content vs inject trigger) — routeInjectQueue only reads the
	// queue and claims the target exactly-once, so it does not interfere with any delivery branch. Decision
	// on the Hook FIFO; delivery dispatched off it (R5). MD-final can fire multiple times per turn — the
	// exactly-once claim inside routeInjectQueue is the duplicate guard.
	if p.Final && p.TmuxTarget != "" && bs.InjectQueue.HasItems(p.TmuxTarget) {
		cb.RouteInjectQueue(bs, p.TmuxTarget, "")
	}
	logDelta := func() {
		logger.Info(fmt.Sprintf("MessageDisplay delta: session=%s message_id=%s turn_id=%s index=%d final=%v delta=%s",
			p.SessionID, p.MessageID, p.TurnID, p.Index, p.Final, p.Delta))
	}
	// Hot path: existing message_id → append without resolving chat (no LoadCredentials per delta). While the
	// session is Stopped, AppendExisting routes the delta through the post-Stop state machine (commit 18) and
	// returns a PostStopAction that drives the follow-up here (arm the NEW stream / flush on completion / drop
	// a STOP-COPY). A late MD is NO LONGER one-shot direct-sent — it becomes a proper order-gated stream.
	if handled, dropped, post := bs.Streams.AppendExisting(p.SessionID, p.MessageID, p.TurnID, p.Index, p.Delta, p.Final); handled {
		switch post {
		case stores.PostStopDrop:
			logger.Info(fmt.Sprintf("post-stop late MD dropped (stop-copy): session=%s message_id=%s turn_id=%s index=%d",
				p.SessionID, p.MessageID, p.TurnID, p.Index))
			return
		case stores.PostStopDefer:
			return // empty/whitespace delta on an unclassified placeholder — nothing to render yet
		case stores.PostStopNeedsArm:
			// A genuinely-new post-Stop message gained content (P2 arming): resolve chat OUTSIDE locks, then
			// ArmStopped installs the full metadata + appends ss.Order under one DataMu hold. ResolveChat failure
			// removes the entry (map + Order sweep). If already complete (single-delta), flush immediately (P5).
			if p.TmuxTarget != "" && bs.SessionState.FindInfoByTarget(p.TmuxTarget) == nil {
				bs.SessionState.Add(p.SessionID, p.TmuxTarget, p.CWD, p.TranscriptPath)
			}
			chat, _, topicID := cb.ResolveChat(bs, p.TmuxTarget)
			if chat == nil {
				bs.Streams.DropStopped(p.SessionID, p.MessageID)
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
			_, complete := bs.Streams.ArmStopped(p.SessionID, p.MessageID, stores.StreamMeta{
				MessageID: p.MessageID, TurnID: p.TurnID, PromptID: p.PromptID, TmuxTarget: p.TmuxTarget,
				Project: p.Project, CWD: cwd, AgentName: agentName, Backend: backend, ChatID: chat.ID, TopicID: topicID,
			})
			logger.Info(fmt.Sprintf("post-stop late MD accepted (stream): session=%s message_id=%s turn_id=%s index=%d",
				p.SessionID, p.MessageID, p.TurnID, p.Index))
			if complete {
				cb.StreamFlush(bs, p.SessionID, true)
			}
			return
		case stores.PostStopArmedComplete:
			logger.Info(fmt.Sprintf("post-stop late MD accepted (stream): session=%s message_id=%s turn_id=%s index=%d",
				p.SessionID, p.MessageID, p.TurnID, p.Index))
			cb.StreamFlush(bs, p.SessionID, true) // completion boundary (P5): relabel + TryClose
			return
		case stores.PostStopArmedProgress:
			logger.Info(fmt.Sprintf("post-stop late MD accepted (stream): session=%s message_id=%s turn_id=%s index=%d",
				p.SessionID, p.MessageID, p.TurnID, p.Index))
			return // ticker renders the non-final progress (order-gated)
		default: // PostStopNone — not stopped, or a known-sealed / closed drop
			if dropped {
				logger.Info(fmt.Sprintf("MessageDisplay delta dropped: session=%s message_id=%s turn_id=%s index=%d reason=closed_stopped_or_sealed",
					p.SessionID, p.MessageID, p.TurnID, p.Index))
			} else {
				logDelta()
			}
			return
		}
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
	// AppendDelta sets the per-session NewTextSinceTool flag when this is a new message bubble (f25: that flag
	// now gates ONLY the pre-tool wait-skip; the compact-tool cycle reset keys on SendBelowSinceTool, the
	// placement signal set in flushSession p3). The reset still happens IN the PreToolUse FIFO handler,
	// serialized with Get/Store — no off-FIFO CompactTools.Reset here (that was the race).
	bs.Streams.AppendDelta(p.SessionID, stores.StreamMeta{
		MessageID:  p.MessageID,
		TurnID:     p.TurnID,
		PromptID:   p.PromptID,
		TmuxTarget: p.TmuxTarget,
		Project:    p.Project,
		CWD:        cwd,
		AgentName:  agentName,
		Backend:    backend,
		ChatID:     chat.ID,
		TopicID:    topicID,
	}, p.Index, p.Delta, p.Final)
	logDelta()
}

// insertBeforeLastDetails inserts text before the last </details> in body.
// If </details> is not found, falls back to appending (for safety on non-rich bodies).
func insertBeforeLastDetails(body, text string) string {
	idx := strings.LastIndex(body, "</details>")
	if idx < 0 {
		return body + text
	}
	return body[:idx] + text + body[idx:]
}

// enqueueToolResultEdit enqueues the msg:tool-result op onto the Message FIFO (S9). The op resolves the TG
// msg id via MsgIDMap.Get (a missing mapping logs + skips — NO recovery send), applies the result edit at the
// resolved id, logs the canonical "PostToolUse: updated msg_id=" marker, then Delete(internalID) in-op so the
// mapping delete is ordered AFTER the tool-send op that Set it. inline annotates the fast-tool path (PostToolUse
// beat PreToolUse). Exactly one result edit per tool_use_id — the caller already Deleted the ToolUseMsgs entry.
func enqueueToolResultEdit(bs *types.BotState, sessionID, project, cwd, tmuxTarget, toolName, agentName, backend, toolUseID string, entry *stores.ToolUseMsgEntry, toolResponse json.RawMessage, inline bool) {
	internalID := entry.InternalID
	bs.MessageQueue.DispatchAsync(sessionID, "msg:tool-result", func() error {
		tgMsgID, ok := bs.MsgIDMap.Get(internalID)
		if !ok {
			logger.Info(fmt.Sprintf("PostToolUse: tool-result skipped, no mapping session=%s internal_id=%d tool=%s tool_use_id=%s", sessionID, internalID, toolName, toolUseID))
			return nil
		}
		rl := applyToolResultEditAt(bs, sessionID, project, cwd, tmuxTarget, toolName, agentName, backend, tgMsgID, entry, toolResponse)
		if inline {
			logger.Info(fmt.Sprintf("PostToolUse: updated msg_id=%d tool=%s tool_use_id=%s result_len=%d (inline: PostToolUse beat PreToolUse under async)", tgMsgID, toolName, toolUseID, rl))
		} else {
			logger.Info(fmt.Sprintf("PostToolUse: updated msg_id=%d tool=%s tool_use_id=%s result_len=%d", tgMsgID, toolName, toolUseID, rl))
		}
		bs.MsgIDMap.Delete(internalID)
		return nil
	})
}

// applyToolResultEditAt edits an existing standard-mode tool-notification message (at the resolved tgMsgID) to
// append the tool result. Runs INSIDE the msg:tool-result op on the Message FIFO. Returns result len.
func applyToolResultEditAt(bs *types.BotState, sessionID, project, cwd, tmuxTarget, toolName, agentName, backend string, tgMsgID int, entry *stores.ToolUseMsgEntry, toolResponse json.RawMessage) int {
	resultText := notify.BuildToolResultText(toolName, toolResponse)
	// Insert result inside the <details> block (before closing </details>) so it expands with the args.
	combinedBody := insertBeforeLastDetails(entry.Body, "\n\n"+resultText)
	postBackend := "cc"
	if backend != "" {
		postBackend = backend
	}
	nd := notify.NotificationData{
		Event: "ToolUse", Project: project, CWD: cwd,
		TmuxTarget: tmuxTarget, ToolName: toolName,
		AgentName: agentName, Backend: postBackend,
		CLICommand:     helpers.GetPaneCLICommand(tmuxTarget),
		ContextUsedPct: -1,
	}
	if usedPct, usedTokens, windowSize, ctxOk := helpers.ReadContextUsage(sessionID); ctxOk {
		nd.ContextUsedPct = usedPct
		nd.ContextUsedTokens = usedTokens
		nd.ContextWindowSize = windowSize
	}
	postCfg, _ := config.LoadAppConfig()
	// Use RichMaxRunes budget (tool notifications are now sent via rich Bot API)
	richMax := 30000
	if postCfg.RichMaxRunes > 0 {
		richMax = postCfg.RichMaxRunes
	}
	maxBodyRunes := richMax - notify.HeaderLen(nd) - 100
	chunks := helpers.SplitBody(combinedBody, maxBodyRunes)
	editMsg := &tele.Message{ID: tgMsgID, Chat: &tele.Chat{ID: entry.ChatID}}
	// detailsReplacer strips <details>/<summary> tags for the legacy-safe LegacyHTML fallback.
	detailsReplacer := strings.NewReplacer(
		"<details>", "", "</details>", "",
		"<summary>", "", "</summary>", "",
	)
	if len(chunks) <= 1 {
		nd.Body = combinedBody
		richText := notify.BuildNotificationText(nd)
		legacyText := detailsReplacer.Replace(richText)
		helpers.RetryEditRich(bs.Bot, editMsg, richText, helpers.RichSendOpts{
			SkipEntityDetection: true,
			LegacyHTML:          legacyText,
		})
	} else {
		nd.Body = chunks[0]
		nd.Page = 1
		nd.TotalPages = len(chunks)
		kb := helpers.BuildPageKeyboard(1, len(chunks))
		richText := notify.BuildNotificationText(nd)
		legacyText := detailsReplacer.Replace(richText)
		helpers.RetryEditRich(bs.Bot, editMsg, richText, helpers.RichSendOpts{
			SkipEntityDetection: true,
			Markup:              kb,
			LegacyHTML:          legacyText,
		})
		bs.Pages.Store(tgMsgID, sessionID, &stores.PageEntry{
			Chunks: chunks, Event: "ToolUse", Project: project,
			CWD: cwd, TmuxTarget: tmuxTarget, ChatID: entry.ChatID,
			Rich:              true,
			CLICommand:        nd.CLICommand,
			AgentName:         nd.AgentName,
			Backend:           nd.Backend,
			ContextUsedPct:    nd.ContextUsedPct,
			ContextUsedTokens: nd.ContextUsedTokens,
			ContextWindowSize: nd.ContextWindowSize,
		})
	}
	return len(resultText)
}
