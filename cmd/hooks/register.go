package hooks

import (
	"fmt"
	"net/http"
	"os/exec"
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
	mux.HandleFunc("/pending/notify", pendingNotifyHandler(bs, cb))
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
		logger.Info(fmt.Sprintf("Raw hook payload [%s]: %s", event, string(raw)))
		// Strip socket suffix so internal stores use bare pane IDs (e.g. %859 not %859@/tmp/...)
		p.TmuxTarget = notify.FormatPaneID(p.TmuxTarget)
		// Re-register session on any hook event (survives bot restart)
		// Exclude SessionStart (handled inside switch after filter) and SessionEnd
		if event != "SessionEnd" && event != "SessionStart" && p.SessionID != "" && p.TmuxTarget != "" {
			bs.SessionState.Add(p.SessionID, p.TmuxTarget, p.CWD, p.TranscriptPath)
			if p.Backend != "" {
				bs.SessionState.SetBackend(p.SessionID, p.Backend)
			}
		}
		if p.SessionID != "" {
			mu := GetHookSessionLock(bs, p.SessionID)
			mu.Lock()
			defer mu.Unlock()
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
					sNameBytes, _ := exec.Command("tmux", "display-message", "-p", "-t", target.PaneID, "#{session_name}").Output()
					if strings.HasPrefix(strings.TrimSpace(string(sNameBytes)), "tg-cli-usage-") {
						break
					}
				}
			}
			if chat == nil || p.TmuxTarget == "" {
				w.WriteHeader(200)
				return
			}
			var body string
			if p.Source == "resume" && p.TranscriptPath != "" {
				body = helpers.ReadLastAssistantText(p.TranscriptPath, 500)
			}
			text := notify.BuildNotificationText(notify.NotificationData{
				Event: "SessionStart", Project: p.Project, CWD: cwdForRoute, TmuxTarget: p.TmuxTarget, Body: body,
				AgentName: hookAgentName,
			})
			var sessionStartOpts []interface{}
			sessionStartOpts = append(sessionStartOpts, tele.ModeHTML)
			if hookTopicID > 0 {
				sessionStartOpts = append(sessionStartOpts, &tele.SendOptions{ThreadID: hookTopicID})
			}
			helpers.RetrySend(bs.Bot, chat, text, sessionStartOpts...)
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
			bs.Pages.CleanupSession(p.SessionID)
			bs.SessionCounts.Cleanup(p.SessionID)
			CleanPendingFilesBySession(p.SessionID)
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
				paneID := notify.FormatPaneID(p.TmuxTarget)
				if err := exec.Command("tmux", "kill-pane", "-t", paneID).Run(); err != nil {
					logger.Error(fmt.Sprintf("SessionEnd kill-pane failed: target=%s err=%v", paneID, err))
				} else {
					logger.Info(fmt.Sprintf("SessionEnd kill-pane: target=%s (exit API)", paneID))
				}
			}
		case "UserPromptSubmit":
			CancelPendingFilesBySession(bs, p.SessionID)
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
				bs.ReactionTracker.PromotePending(bs.Bot, p.TmuxTarget)
				logger.Debug(fmt.Sprintf("Promoted pending reactions for tmux target: %s", p.TmuxTarget))
				bs.InjectConfirm.Confirm(p.TmuxTarget)
			}
		case "Stop":
			if p.TmuxTarget != "" {
				bs.HookRunning.SetIdle(p.TmuxTarget)
				cb.TypingLog("state: event=Stop target=%s state=idle", p.TmuxTarget)
				bs.StopCooldown.Record(p.TmuxTarget)
			}
			CancelPendingFilesBySession(bs, p.SessionID)
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
				if body != "" {
					cb.SendEventNotification(bs, chat, chatID, p.SessionID, "Stop", p.Project, cwdForRoute, p.TmuxTarget, body, "", hookAgentName, hookTopicID)
				}
				bs.SessionWatch.Notify(hookAgentName, stores.WatchEvent{
					Event:   "Stop",
					Agent:   hookAgentName,
					Summary: "Task completed",
					Detail:  helpers.TruncateStr(body, 500),
				})
				// Flush queued inject items (messages queued while CC was busy)
				if p.TmuxTarget != "" {
					cb.FlushInjectQueue(bs, p.TmuxTarget)
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
			CancelPendingFilesBySession(bs, p.SessionID)
			// Skip TG notifications for subagent tool calls
			if p.AgentID != "" {
				break
			}
			if p.TmuxTarget != "" {
				bs.ReactionTracker.PromotePending(bs.Bot, p.TmuxTarget)
			}
			// PreToolUse: send intermediate notification
			// Skip processTranscriptUpdates for AskUserQuestion — /pending/notify handler will call it
			// to avoid race condition where both paths compete for sessionCounts
			if chat != nil && p.ToolName != "AskUserQuestion" {
				body := cb.ProcessTranscriptUpdates(bs, p.SessionID, p.TranscriptPath)
				if body != "" {
					cb.SendEventNotification(bs, chat, chatID, p.SessionID, "PreToolUse", p.Project, cwdForRoute, p.TmuxTarget, body, "", hookAgentName, hookTopicID)
				}
			}
			// Send tool detail notification if configured
			toolCfg, _ := config.LoadAppConfig()
			if chat != nil && helpers.ShouldNotifyTool(p.ToolName, toolCfg.ToolNotifyEnabled, toolCfg.ToolNotifyList) {
				toolText := notify.BuildToolNotifyText(p.ToolName, p.ToolInput, cwdForRoute)
				if toolText != "" {
					cb.SendEventNotification(bs, chat, chatID, p.SessionID, "ToolUse", p.Project, cwdForRoute, p.TmuxTarget, toolText, p.ToolName, hookAgentName, hookTopicID)
				}
			}
		case "PostToolUse":
			// Codex-specific: tool execution completed
			if chat != nil && p.ToolName != "" {
				// Send optional tool completion notification (similar to ToolUse notify)
				cfg, cfgErr := config.LoadAppConfig()
				if cfgErr != nil {
					break
				}
				if cfg.ToolNotifyEnabled == nil || *cfg.ToolNotifyEnabled {
					shouldNotify := false
					for _, t := range cfg.ToolNotifyList {
						if strings.EqualFold(t, p.ToolName) {
							shouldNotify = true
							break
						}
					}
					if shouldNotify {
						logger.Info(fmt.Sprintf("PostToolUse notification: tool=%s target=%s", p.ToolName, p.TmuxTarget))
					}
				}
			}
		case "PermissionRequest":
			// PermissionRequest is now handled via file-based communication
			// hook.go writes pending file and polls for answer
			// This handler is no longer used by hook.go, but kept for backward compatibility
			logger.Info(fmt.Sprintf("PermissionRequest received via HTTP (legacy path): tool=%s", p.ToolName))
			w.WriteHeader(200)
			return
		default:
			// Unknown event — send notification if possible
			if chat != nil {
				body := cb.ProcessTranscriptUpdates(bs, p.SessionID, p.TranscriptPath)
				cb.SendEventNotification(bs, chat, chatID, p.SessionID, event, p.Project, cwdForRoute, p.TmuxTarget, body, "", hookAgentName, hookTopicID)
			}
		}
		w.WriteHeader(200)
	})
}
