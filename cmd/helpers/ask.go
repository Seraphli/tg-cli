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
	"github.com/Seraphli/tg-cli/internal/markdown"
	tele "gopkg.in/telebot.v3"
)

// ErrInjectNotConfirmed is returned when inject confirmation fails (CapturePane miss + channel timeout).
var ErrInjectNotConfirmed = fmt.Errorf("inject not confirmed")

// DoRespondAsk responds to AskUserQuestion: resolve via ResolveIfUnresolved + EditOrDefer.
// Uses FindByMsgIDSnapshot (TG-button path — safe because msgID is real Telegram callback ID).
// EditFunc uses PendingMsgStore-provided msgID and chatID parameters.
func DoRespondAsk(
	bot *tele.Bot,
	pendingWait *stores.PendingWaitStore,
	reactionTracker *stores.ReactionTrackerStore,
	pendingMsgStore *stores.PendingMsgStore,
	msgID int,
	answers map[string]string,
	frozenLabel string,
) error {
	snap, ok := pendingWait.FindByMsgIDSnapshot(msgID)
	if !ok {
		return fmt.Errorf("pending entry not found")
	}
	uuid := snap.UUID
	waitEntry, wok := pendingWait.Get(uuid)
	if !wok {
		cleanupAskState(bot, pendingWait, msgID, uuid, "wait entry missing")
		return fmt.Errorf("hook dead (stale pending)")
	}
	ccOutput := BuildAskCCOutput(waitEntry.Payload, answers)
	// Pre-build frozen markup before CAS using snap.Questions
	var frozenMarkup *tele.ReplyMarkup
	var msgText string
	if len(snap.Questions) > 0 {
		frozenMarkup = BuildFrozenMarkup(snap.Questions, frozenLabel)
		msgText = snap.MsgText
		reactionTracker.RecordPending(bot, snap.TmuxTarget, snap.ChatID, msgID)
	}
	won, _, _ := pendingWait.ResolveIfUnresolved(uuid, stores.WaitEvent{
		Type:   "answer",
		Output: ccOutput,
	})
	if !won {
		return nil
	}
	if frozenMarkup != nil && pendingMsgStore != nil {
		capturedMarkup := frozenMarkup
		capturedLabel := frozenLabel
		capturedRich := snap.Rich
		pendingMsgStore.EditOrDefer(uuid, func(eID int, chatID int64, editMsgText string, topicID int) {
			editMsg := &tele.Message{ID: eID, Chat: &tele.Chat{ID: chatID}}
			_, err := RetryFreezeEditAuto(bot, editMsg, capturedRich, editMsgText, capturedMarkup)
			if err != nil {
				logger.Error(fmt.Sprintf("DoRespondAsk: EDIT failed msg_id=%d label=%s err=%v", eID, capturedLabel, err))
			} else {
				logger.Info(fmt.Sprintf("DoRespondAsk: EDIT completed msg_id=%d label=%s", eID, capturedLabel))
			}
		})
	} else if frozenMarkup != nil {
		// Fallback: direct edit using snap coordinates
		editMsg := &tele.Message{ID: snap.MsgID, Chat: &tele.Chat{ID: snap.ChatID}}
		RetryFreezeEditAuto(bot, editMsg, snap.Rich, msgText, frozenMarkup)
	}
	logger.Info(fmt.Sprintf("AskUserQuestion responded: msg_id=%d uuid=%s answers=%v", msgID, uuid, answers))
	return nil
}

// DoCancelAsk cancels an AskUserQuestion: ResolveIfUnresolved + ESC + EditOrDefer.
// Uses FindByMsgIDSnapshot (TG-button path — safe because msgID is real Telegram callback ID).
// EditFunc uses PendingMsgStore-provided msgID and chatID parameters.
func DoCancelAsk(
	bot *tele.Bot,
	pendingWait *stores.PendingWaitStore,
	pendingMsgStore *stores.PendingMsgStore,
	extractTarget func(string) (*injector.TmuxTarget, error),
	msgID int,
) string {
	snap, ok := pendingWait.FindByMsgIDSnapshot(msgID)
	if !ok {
		return ""
	}
	uuid := snap.UUID
	// Pre-build frozen markup before CAS using snap.Questions
	var frozenMarkup *tele.ReplyMarkup
	var msgText string
	if len(snap.Questions) > 0 {
		frozenMarkup = BuildFrozenMarkup(snap.Questions, "❌ Cancelled")
		msgText = snap.MsgText
		targetPtr, err := extractTarget(snap.MsgText)
		if err == nil && targetPtr != nil {
			injector.SendKeys(*targetPtr, "Escape")
		}
	}
	won, _, _ := pendingWait.ResolveIfUnresolved(uuid, stores.WaitEvent{Type: "cancel"})
	if won && frozenMarkup != nil {
		if pendingMsgStore != nil {
			capturedMarkup := frozenMarkup
			capturedRich := snap.Rich
			pendingMsgStore.EditOrDefer(uuid, func(eID int, eChatID int64, editMsgText string, topicID int) {
				editMsg := &tele.Message{ID: eID, Chat: &tele.Chat{ID: eChatID}}
				_, err := RetryFreezeEditAuto(bot, editMsg, capturedRich, editMsgText, capturedMarkup)
				if err != nil {
					logger.Error(fmt.Sprintf("DoCancelAsk: EDIT failed msg_id=%d err=%v", eID, err))
				} else {
					logger.Info(fmt.Sprintf("DoCancelAsk: EDIT completed msg_id=%d", eID))
				}
			})
		} else {
			editMsg := &tele.Message{ID: snap.MsgID, Chat: &tele.Chat{ID: snap.ChatID}}
			RetryFreezeEditAuto(bot, editMsg, snap.Rich, msgText, frozenMarkup)
		}
	}
	logger.Info(fmt.Sprintf("AskUserQuestion cancelled: msg_id=%d uuid=%s", msgID, uuid))
	return uuid
}

// DoChatAsk handles chat mode for AskUserQuestion: ResolveIfUnresolved + EditOrDefer.
// Uses FindByMsgIDSnapshot (TG-button path — safe because msgID is real Telegram callback ID).
// EditFunc uses PendingMsgStore-provided msgID and chatID parameters.
func DoChatAsk(
	bot *tele.Bot,
	pendingWait *stores.PendingWaitStore,
	reactionTracker *stores.ReactionTrackerStore,
	pendingMsgStore *stores.PendingMsgStore,
	msgID int,
) error {
	snap, ok := pendingWait.FindByMsgIDSnapshot(msgID)
	if !ok {
		return fmt.Errorf("pending entry not found")
	}
	uuid := snap.UUID
	waitEntry, wok := pendingWait.Get(uuid)
	if !wok {
		cleanupAskState(bot, pendingWait, msgID, uuid, "wait entry missing on chat button")
		return fmt.Errorf("question expired")
	}
	answers := map[string]string{"__chat": "true"}
	ccOutput := BuildAskCCOutput(waitEntry.Payload, answers)
	// Pre-build frozen markup before CAS using snap.Questions
	var frozenMarkup *tele.ReplyMarkup
	var msgText string
	if len(snap.Questions) > 0 {
		frozenMarkup = BuildFrozenMarkup(snap.Questions, "💬 Chat mode selected")
		msgText = snap.MsgText
		reactionTracker.RecordPending(bot, snap.TmuxTarget, snap.ChatID, msgID)
	}
	won, _, _ := pendingWait.ResolveIfUnresolved(uuid, stores.WaitEvent{
		Type:   "answer",
		Output: ccOutput,
	})
	if !won {
		return nil
	}
	if frozenMarkup != nil && pendingMsgStore != nil {
		capturedMarkup := frozenMarkup
		capturedRich := snap.Rich
		pendingMsgStore.EditOrDefer(uuid, func(eID int, chatID int64, editMsgText string, topicID int) {
			editMsg := &tele.Message{ID: eID, Chat: &tele.Chat{ID: chatID}}
			_, err := RetryFreezeEditAuto(bot, editMsg, capturedRich, editMsgText, capturedMarkup)
			if err != nil {
				logger.Error(fmt.Sprintf("DoChatAsk: EDIT failed msg_id=%d err=%v", eID, err))
			} else {
				logger.Info(fmt.Sprintf("DoChatAsk: EDIT completed msg_id=%d", eID))
			}
		})
	} else if frozenMarkup != nil {
		// Fallback: direct edit using snap coordinates
		editMsg := &tele.Message{ID: snap.MsgID, Chat: &tele.Chat{ID: snap.ChatID}}
		RetryFreezeEditAuto(bot, editMsg, snap.Rich, msgText, frozenMarkup)
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
	PendingWait      *stores.PendingWaitStore
	InjectQueue      *stores.InjectQueueStore
	InjectConfirm    *stores.InjectConfirmStore
	StopCooldown     *stores.StopCooldownStore
	ReactionTracker  *stores.ReactionTrackerStore
	SessionState     *stores.SessionStateStore
	HookSessionLocks *sync.Map
	SessionEvents    *stores.SessionEventStore
	PendingMsgStore  *stores.PendingMsgStore // Deferred EDIT after AskQ answer
	ResolveChat      func(string) (*tele.Chat, string, int)
	FormatPaneID     func(string) string
	Force            bool   // Skip busy check — used by flushInjectQueue
	AltSnippet       string // Alternative snippet for CapturePane (e.g. "[Image" for image inject)
	// AwaitAskQReady, when set, makes the phase-1 state check BOUNDED-WAIT (R1) for the PendingWait
	// AskUserQuestion snapshot to appear before deciding. PendingWait registers in /pending/connect (NOT
	// the PreToolUse hook), so at AskQ-custom-reply routing time the entry may not exist yet. Used only by
	// the event-driven inject-queue router in AskQ-custom-reply mode.
	AwaitAskQReady bool
	// InjectDiag, if set, captures the pane immediately before/after the Enter keypress on the
	// direct-inject path (flush only) for diagnostics. phase is "before-enter"/"after-enter".
	InjectDiag func(phase, pane string)
	// Requeued, when non-nil, is set to true if phase1 re-queued the text (CC busy or PermissionRequest
	// pending) instead of injecting/answering. Lets a caller (deliverInjectQueue) log/notify accurately
	// rather than reporting a re-queue as "Injected". Nil for callers that do not care.
	Requeued *bool
}

// injectResult carries the outcome of safeInjectPhase1 to safeInjectPhase2.
type injectResult struct {
	err           error
	ch            chan bool
	confirmType   string // "askq", "prompt", "codex_slash", ""
	shouldSubmit  bool
	captureTarget injector.TmuxTarget
	snippet       string
	altSnippet    string
	submitted     bool // f29 C: codex_slash — the phase1 transaction pressed Enter (composer confirmed)
}

// isCodexSlash reports whether text is a codex slash-command — the scope of the f29 C inject-confirmation
// transaction; every other case keeps the existing UPS/capture path. Source-faithful to the codex 0.144.x
// slash grammar (codex-rs tui/src/bottom_pane/prompt_args.rs parse_slash_name +
// tui/src/slash_input.rs validate_submission/inline_command): the input must LITERALLY start with "/"
// (codex treats space-led input as ordinary text — no trimming), the first whitespace-delimited token
// must name a non-empty command (parse_slash_name returns None for a bare "/"), and the name itself may
// not contain "/" — which excludes multi-slash file paths like /tmp/x.jpg. Root-path single-slash image
// paths (/image.jpg) still pass this text predicate and are excluded at the gate by codexSlashGate's
// AltSnippet image bypass.
func isCodexSlash(backend, text string) bool {
	if backend != "codex" || !strings.HasPrefix(text, "/") {
		return false
	}
	token := strings.Fields(text)[0]
	return len(token) > 1 && !strings.Contains(token[1:], "/")
}

// codexSlashGate decides whether the f29 C codex-slash transaction runs for an inject. Image injects are
// bypassed unconditionally: AltSnippet is set ONLY by the image-inject path (messages.go, "[Image"), and
// codex renders a valid pasted image path as an attachment chip — the composer never shows the pasted
// text, so compose-confirm could never match and the transaction would always veto with a false
// ErrInjectNotConfirmed (the attempt-5 codex phase14 regression). A root-path image like /image.jpg is a
// legal single-slash token the text predicate alone cannot exclude, hence this explicit bypass.
func codexSlashGate(backend, text, altSnippet string) bool {
	return altSnippet == "" && isCodexSlash(backend, text)
}

// codexComposerHasText reports whether the codex composer (a line led by the "›" prompt char) currently
// shows our pasted snippet — the compose-confirm predicate for the f29 C codex-slash transaction. Scans
// bottom-up so the live composer line (nearest the bottom) is matched, mirroring the phase2 prompt scan.
func codexComposerHasText(pane, snippet string) bool {
	lines := strings.Split(pane, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		raw := strings.TrimRight(lines[i], " \t")
		j := strings.Index(raw, "›")
		if j < 0 {
			continue
		}
		after := strings.TrimLeft(raw[j+len("›"):], " \xc2\xa0")
		if after != "" && strings.Contains(after, snippet) {
			return true
		}
	}
	return false
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
	// R1 (AskQ handshake): the AskQ-custom-reply router routes on PreToolUse(AskUserQuestion), but the
	// PendingWait AskQ entry registers in /pending/connect — it may not exist yet. Bounded-wait (up to 3s)
	// for the AskQ snapshot to appear before the state check below, so we do not fall through to a direct
	// inject that races the picker. If a non-AskQ (PermissionRequest) entry appears first, stop waiting —
	// the permission guard below keeps the queue. Polling releases the session lock so /pending/connect
	// (which needs the same lock via hook processing) is not blocked.
	if p.AwaitAskQReady {
		waitDeadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(waitDeadline) {
			snap, ok := p.PendingWait.FindByTmuxTarget(tmuxTarget)
			if ok && snap != nil {
				break // an entry (AskQ or PermissionRequest) is registered — proceed to the state check
			}
			if sessionMu != nil {
				sessionMu.Unlock()
			}
			time.Sleep(150 * time.Millisecond)
			if sessionMu != nil {
				sessionMu.Lock()
			}
		}
	}
	// PRE-INJECT: check if there's a pending entry (AskQ or PermissionRequest) via PendingWait
	pwSnap, hasPending := p.PendingWait.FindByTmuxTarget(tmuxTarget)
	hasAskQ := hasPending && pwSnap != nil && pwSnap.ToolName == "AskUserQuestion"
	if !p.Force && IsSessionRunning(tmuxTarget) && !hasPending {
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
			// Fix 19: rich notification (RetrySendRich/RetryEditRich) so long queued content is not
			// truncated by the 4096 plain-text cap; HTML-escape the user text for correct rich rendering.
			notifyText := fmt.Sprintf("⏳ Queued [%s] (%d)\n📟 %s\n──────\n%s\n──────", queueID, count, p.FormatPaneID(tmuxTarget), markdown.EscapeHTML(truncateQueueTexts(allTexts, 3500)))
			if existingMsgID, ok := p.InjectQueue.GetNotifyMsg(tmuxTarget); ok {
				editMsg := &tele.Message{ID: existingMsgID, Chat: chat}
				RetryEditRich(p.Bot, editMsg, notifyText, RichSendOpts{})
			} else {
				sent, _ := RetrySendRich(p.Bot, chat, notifyText, RichSendOpts{TopicID: topicID})
				if sent != nil {
					p.InjectQueue.SetNotifyMsg(tmuxTarget, sent.ID)
				}
			}
		}
		if sessionMu != nil {
			sessionMu.Unlock()
		}
		if p.Requeued != nil {
			*p.Requeued = true
		}
		return injectResult{}
	}
	// Answer pending AskUserQuestion via PendingWait (atomic CAS)
	if hasAskQ && pwSnap != nil {
		waitEntry, wok := p.PendingWait.Get(pwSnap.UUID)
		if !wok {
			// Entry gone — fall through to direct inject
		} else {
			answers := make(map[string]string)
			// Find question text from snap.Questions if available
			if len(pwSnap.Questions) > 0 {
				answers[pwSnap.Questions[0].QuestionText] = text
			} else {
				answers["question"] = text
			}
			ccOutput := BuildAskCCOutput(waitEntry.Payload, answers)
			// Use ResolveIfUnresolved for atomic CAS
			won, _, _ := p.PendingWait.ResolveIfUnresolved(pwSnap.UUID, stores.WaitEvent{
				Type:   "answer",
				Output: ccOutput,
			})
			if won {
				if sessionMu != nil {
					sessionMu.Unlock()
				}
				// EditOrDefer EDIT with pre-built frozen markup using snap.Questions.
				// Runs inline (SessionEvents is nil inside safe-inject phase) because we set p.SessionEvents=nil above.
				if p.PendingMsgStore != nil {
					var frozenMarkup *tele.ReplyMarkup
					if len(pwSnap.Questions) > 0 {
						frozenMarkup = BuildFrozenMarkup(pwSnap.Questions, "✅ Custom reply")
					}
					if frozenMarkup != nil {
						capturedMarkup := frozenMarkup
						capturedUUID := pwSnap.UUID
						capturedRich := pwSnap.Rich
						p.PendingMsgStore.EditOrDefer(capturedUUID, func(msgID int, chatID int64, editMsgText string, topicID int) {
							editMsg := &tele.Message{ID: msgID, Chat: &tele.Chat{ID: chatID}}
							_, err := RetryFreezeEditAuto(p.Bot, editMsg, capturedRich, editMsgText, capturedMarkup)
							if err != nil {
								logger.Error(fmt.Sprintf("safeInjectText: AskQ EDIT failed msg_id=%d err=%v", msgID, err))
							} else {
								logger.Info(fmt.Sprintf("safeInjectText: AskQ EDIT completed msg_id=%d", msgID))
							}
						})
					}
				} else {
					// Fallback: direct edit using snap coordinates
					if len(pwSnap.Questions) > 0 {
						editMsg := &tele.Message{ID: pwSnap.MsgID, Chat: &tele.Chat{ID: pwSnap.ChatID}}
						RetryFreezeEditAuto(p.Bot, editMsg, pwSnap.Rich, pwSnap.MsgText, BuildFrozenMarkup(pwSnap.Questions, "✅ Custom reply"))
					}
				}
				logger.Info(fmt.Sprintf("safeInjectText: answered AskUserQuestion uuid=%s text=%s", pwSnap.UUID, text))
				ch := p.InjectConfirm.Register(tmuxTarget, stores.ConfirmAskAnswered, text)
				return injectResult{ch: ch, confirmType: "askq"}
			}
			// CAS lost — another resolver won, fall through to direct inject
		}
	}
	// PermissionRequest pending — pwSnap exists but ToolName is not AskUserQuestion
	if pwSnap != nil && pwSnap.ToolName != "AskUserQuestion" {
		chat, chatIDStr, topicID := p.ResolveChat(tmuxTarget)
		chatIDInt := int64(0)
		for _, c := range chatIDStr {
			if c >= '0' && c <= '9' {
				chatIDInt = chatIDInt*10 + int64(c-'0')
			}
		}
		p.InjectQueue.Enqueue(tmuxTarget, stores.InjectItem{Text: text, ChatID: chatIDInt, TopicID: topicID})
		count := p.InjectQueue.ItemCount(tmuxTarget)
		logger.Info(fmt.Sprintf("safeInjectText: PermissionRequest pending, queued for target=%s count=%d text=%s", tmuxTarget, count, text))
		if chat != nil {
			allTexts := p.InjectQueue.GetTexts(tmuxTarget)
			queueID := p.InjectQueue.GetInjectID(tmuxTarget)
			// Fix 19: rich notification (RetrySendRich/RetryEditRich) so long queued content is not
			// truncated by the 4096 plain-text cap; HTML-escape the user text for correct rich rendering.
			notifyText := fmt.Sprintf("⏳ Queued [%s] (%d)\n📟 %s\n🔒 PermissionRequest pending\n──────\n%s\n──────", queueID, count, p.FormatPaneID(tmuxTarget), markdown.EscapeHTML(truncateQueueTexts(allTexts, 3500)))
			if existingMsgID, ok := p.InjectQueue.GetNotifyMsg(tmuxTarget); ok {
				editMsg := &tele.Message{ID: existingMsgID, Chat: chat}
				RetryEditRich(p.Bot, editMsg, notifyText, RichSendOpts{})
			} else {
				sent, _ := RetrySendRich(p.Bot, chat, notifyText, RichSendOpts{TopicID: topicID})
				if sent != nil {
					p.InjectQueue.SetNotifyMsg(tmuxTarget, sent.ID)
				}
			}
		}
		if sessionMu != nil {
			sessionMu.Unlock()
		}
		if p.Requeued != nil {
			*p.Requeued = true
		}
		return injectResult{}
	}
	logger.Info(fmt.Sprintf("safeInjectText: direct inject path, target=%s text=%s", tmuxTarget, text))
	// Wait for Stop event cooldown before injecting
	p.StopCooldown.WaitIfNeeded(tmuxTarget, 3*time.Second)
	shouldSubmit := len(submit) == 0 || submit[0]
	snippet := text
	if idx := strings.Index(snippet, "\n"); idx >= 0 {
		snippet = snippet[:idx]
	}
	if len(snippet) > 50 {
		snippet = snippet[:50]
	}
	// f29 C: codex slash-command inject confirmation. codex emits NO UserPromptSubmit for a local slash
	// command, and by the time a post-hoc capture runs the composer is already cleared — so the normal path
	// yields a false ErrInjectNotConfirmed even though the command executed. Instead we OWN the Enter: paste
	// WITHOUT Enter and poll the composer for our text UNDER the inject lock (still under sessionMu here, so
	// the busy/pending state-check→mutation stays atomic — no TOCTOU), submit on confirm, then phase2 polls
	// for the Working indicator. Scope: codex backend + leading "/" only; normal prompts keep the UPS path.
	if shouldSubmit && p.SessionState != nil {
		if info := p.SessionState.FindInfoByTarget(tmuxTarget); info != nil && codexSlashGate(info.Backend, text, p.AltSnippet) {
			submitted, injErr := injector.InjectTextConfirmSubmit(target, text, func(pane string) bool {
				return codexComposerHasText(pane, snippet)
			}, 3*time.Second, 250*time.Millisecond)
			if sessionMu != nil {
				sessionMu.Unlock()
			}
			if injErr != nil {
				return injectResult{err: injErr}
			}
			return injectResult{confirmType: "codex_slash", captureTarget: target, snippet: snippet, submitted: submitted}
		}
	}
	ch := p.InjectConfirm.Register(tmuxTarget, stores.ConfirmUserPromptSubmit, text)
	var injErr error
	if p.InjectDiag != nil {
		injErr = injector.InjectTextDiag(target, text, shouldSubmit, p.InjectDiag)
	} else {
		injErr = injector.InjectText(target, text, shouldSubmit)
	}
	if injErr != nil {
		p.InjectConfirm.Cancel(tmuxTarget)
		if sessionMu != nil {
			sessionMu.Unlock()
		}
		return injectResult{err: injErr}
	}
	// Release lock after injection — CapturePane and confirmation wait happen in phase2
	if sessionMu != nil {
		sessionMu.Unlock()
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
				p.ReactionTracker.ClearReactions(p.Bot, tmuxTarget)
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
	if res.confirmType == "codex_slash" {
		// f29 C: the clear+paste+compose-confirm+submit already ran in phase1 under sessionMu. If the
		// composer never showed our text, the transaction pressed nothing → unconfirmed (soft
		// delivery-status) path.
		if !res.submitted {
			logger.Info(fmt.Sprintf("safeInjectText: codex slash compose not confirmed (no submit), target=%s", tmuxTarget))
			return fmt.Errorf("%w for target=%s", ErrInjectNotConfirmed, tmuxTarget)
		}
		// Working-confirm (read-only, OUTSIDE any lock): poll for the codex Working indicator
		// (IsSessionRunning true = the canonical busy pane-title spinner) up to workingTimeout. A
		// TUI-exiting command (/quit, /exit) never shows Working, so it soft-annotates unconfirmed though
		// it executed — known behavior, no special-casing.
		deadline := time.Now().Add(3 * time.Second)
		for {
			if IsSessionRunning(tmuxTarget) {
				p.ReactionTracker.ClearReactions(p.Bot, tmuxTarget)
				logger.Info(fmt.Sprintf("safeInjectText: codex slash inject confirmed (Working), target=%s", tmuxTarget))
				return nil
			}
			if time.Now().After(deadline) {
				logger.Info(fmt.Sprintf("safeInjectText: codex slash inject not confirmed (Working timeout), target=%s", tmuxTarget))
				return fmt.Errorf("%w for target=%s", ErrInjectNotConfirmed, tmuxTarget)
			}
			time.Sleep(250 * time.Millisecond)
		}
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
		p.ReactionTracker.ClearReactions(p.Bot, tmuxTarget)
		logger.Info(fmt.Sprintf("safeInjectText: inject confirmed, target=%s capturePane=%v", tmuxTarget, captureConfirmed))
	} else {
		logger.Info(fmt.Sprintf("safeInjectText: inject not confirmed, target=%s", tmuxTarget))
		return fmt.Errorf("%w for target=%s", ErrInjectNotConfirmed, tmuxTarget)
	}
	if !res.shouldSubmit {
		p.InjectConfirm.Cancel(tmuxTarget)
	}
	return nil
}

// cleanupAskState cleans up bot memory state and freezes TG buttons.
// Uses GetSnapshot(uuid) to look up entry data.
func cleanupAskState(
	bot *tele.Bot,
	pendingWait *stores.PendingWaitStore,
	msgID int,
	uuid string,
	reason string,
) {
	snap, ok := pendingWait.GetSnapshot(uuid)
	if !ok {
		logger.Info(fmt.Sprintf("Stale pending cleanup: msg_id=%d uuid=%s reason=%s (entry not found)", msgID, uuid, reason))
		return
	}
	if !snap.Resolved && len(snap.Questions) > 0 {
		editMsg := &tele.Message{ID: snap.MsgID, Chat: &tele.Chat{ID: snap.ChatID}}
		RetryFreezeEditAuto(bot, editMsg, snap.Rich, snap.MsgText, BuildFrozenMarkup(snap.Questions, "❌ Cancelled"))
	}
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
