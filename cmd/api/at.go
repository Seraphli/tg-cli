package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/Seraphli/tg-cli/cmd/helpers"
	"github.com/Seraphli/tg-cli/cmd/types"
	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/logger"
	tele "gopkg.in/telebot.v3"
)

func registerAt(mux *http.ServeMux, bs *types.BotState) {
	bot := bs.Bot

	mux.HandleFunc("/at/open", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Initiator string `json:"initiator"`
			Target    string `json:"target"`
			Rounds    int    `json:"rounds"`
			Lines     int    `json:"lines"`
			Message   string `json:"message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		initiatorInfo := bs.SessionState.FindByName(req.Initiator)
		if initiatorInfo == nil {
			http.Error(w, "initiator session not found", http.StatusNotFound)
			return
		}
		targetInfo := bs.SessionState.FindByName(req.Target)
		if targetInfo == nil {
			// Send error TG notification to initiator's chat
			chat, _, topicID := helpers.ResolveChat(bs.SessionState, initiatorInfo.TmuxTarget)
			if chat != nil {
				var sendOpts []interface{}
				if topicID > 0 {
					sendOpts = append(sendOpts, &tele.SendOptions{ThreadID: topicID})
				}
				helpers.RetrySend(bot, chat, fmt.Sprintf("❌ @ open failed: target session %q not found", req.Target), sendOpts...)
			}
			http.Error(w, "target session not found", http.StatusNotFound)
			return
		}
		isNew := bs.AtChannels.Open(req.Initiator, req.Target)

		cfg, _ := config.LoadAppConfig()
		displayName := cfg.DisplayName
		if displayName == "" {
			displayName = "User"
		}

		p := buildSafeInjectParams(bs)

		targetChat, _, targetTopicID := helpers.ResolveChat(bs.SessionState, targetInfo.TmuxTarget)
		initiatorChat, _, initiatorTopicID := helpers.ResolveChat(bs.SessionState, initiatorInfo.TmuxTarget)

		if isNew {
			// Read context from initiator's transcript
			rounds := req.Rounds
			if req.Lines == 0 && rounds == 0 {
				rounds = 3
			}
			contextStr, _ := helpers.ReadContextBlock(initiatorInfo.TranscriptPath, rounds, req.Lines, initiatorInfo.Backend, req.Initiator, displayName)

			// Append user's message to context if provided
			if req.Message != "" {
				msgLine := fmt.Sprintf("[%s → %s]: %s", req.Initiator, req.Target, req.Message)
				if contextStr != "" {
					contextStr = contextStr + "\n" + msgLine
				} else {
					contextStr = msgLine
				}
			}

			initEndCmd := helpers.AtEndCommand(bs.ConfigDir, bs.Port, req.Initiator, req.Target)
			targetReplyCmd := helpers.AtReplyCommand(bs.ConfigDir, bs.Port, req.Target, req.Initiator)
			targetEndCmd := helpers.AtEndCommand(bs.ConfigDir, bs.Port, req.Target, req.Initiator)
			// Build initiator message (no-content variant: header + instructions only)
			initiatorInstructions := fmt.Sprintf("`%s` opened a channel to `%s`. `%s` will receive the last %d rounds of your conversation and see your ongoing output until the channel is closed. `%s` can reply to you via this channel. Run `%s` to close the channel.",
				req.Initiator, req.Target, req.Target, rounds, req.Target, initEndCmd)
			initiatorContent := ""
			if req.Message != "" {
				initiatorContent = fmt.Sprintf("[%s → %s]: %s", req.Initiator, req.Target, req.Message)
			}
			initiatorMsg := helpers.BuildAtMsg(req.Initiator, req.Target, initiatorInstructions, initiatorContent)

			// Build target instructions
			targetInstructions := fmt.Sprintf("`%s` opened a channel to you via @ channel. Below is the last %d rounds of conversation from `%s`. You will continue to receive updates from `%s` until the channel is closed. Run `%s` to reply, or `%s` to close the channel.",
				req.Initiator, rounds, req.Initiator, req.Initiator, targetReplyCmd, targetEndCmd)
			// Build target message (full variant: header + instructions + content)
			targetMsg := helpers.BuildAtMsg(req.Initiator, req.Target, targetInstructions, contextStr)

			// Inject target pane only (CLI initiator does not get pane injection per spec scenario 4)
			if err := helpers.SafeInjectText(p, targetInfo.TmuxTarget, targetMsg); err != nil {
				logger.Info(fmt.Sprintf("@ open inject target error: %v", err))
			}

			// TG to initiator
			if initiatorChat != nil {
				var sendOpts []interface{}
				if initiatorTopicID > 0 {
					sendOpts = append(sendOpts, &tele.SendOptions{ThreadID: initiatorTopicID})
				}
				helpers.RetrySend(bot, initiatorChat, initiatorMsg, sendOpts...)
			}

			// TG to target (paginated)
			if targetChat != nil {
				var sendOpts []interface{}
				if targetTopicID > 0 {
					sendOpts = append(sendOpts, &tele.SendOptions{ThreadID: targetTopicID})
				}
				targetHeader := helpers.BuildAtHeader(req.Initiator, req.Target) + "\n---\n" + targetInstructions + "\n---\n"
				helpers.SendPagedForward(bot, targetChat, targetHeader, contextStr, bs.Pages, "", sendOpts...)
			}

			// Auto-forward open message to other existing channels
			otherTargets := bs.AtChannels.GetTargets(req.Initiator)
			for _, other := range otherTargets {
				if other == req.Target {
					continue
				}
				otherInfo := bs.SessionState.FindByName(other)
				if otherInfo == nil {
					continue
				}
				fwdContent := initiatorContent
				if fwdContent == "" {
					fwdContent = fmt.Sprintf("[%s → %s]: @%s", req.Initiator, req.Target, req.Target)
				}
				fwdInstr := fmt.Sprintf("`%s` sent a message to `%s`.", req.Initiator, req.Target)
				fwdMsg := helpers.BuildAtMsg(req.Initiator, other, fwdInstr, fwdContent)
				if err := helpers.SafeInjectText(p, otherInfo.TmuxTarget, fwdMsg); err != nil {
					logger.Info(fmt.Sprintf("@ open auto-forward inject error: %v", err))
				}
				otherChat, _, otherTopicID := helpers.ResolveChat(bs.SessionState, otherInfo.TmuxTarget)
				if otherChat != nil {
					var fwdOpts []interface{}
					if otherTopicID > 0 {
						fwdOpts = append(fwdOpts, &tele.SendOptions{ThreadID: otherTopicID})
					}
					helpers.RetrySend(bot, otherChat, fwdMsg, fwdOpts...)
				}
			}
		} else if req.Message != "" {
			// Existing channel: send message update
			content := fmt.Sprintf("[%s → %s]: %s", req.Initiator, req.Target, req.Message)

			// Build initiator message and TG to initiator (no-instructions variant: header + content)
			initiatorContent := fmt.Sprintf("[%s → %s]: %s", req.Initiator, req.Target, req.Message)
			initiatorMsg := helpers.BuildAtMsg(req.Initiator, req.Target, "", initiatorContent)
			if initiatorChat != nil {
				var sendOpts []interface{}
				if initiatorTopicID > 0 {
					sendOpts = append(sendOpts, &tele.SendOptions{ThreadID: initiatorTopicID})
				}
				helpers.RetrySend(bot, initiatorChat, initiatorMsg, sendOpts...)
			}

			// Build target instructions and message
			targetInstr := fmt.Sprintf("`%s` sent you a message via @ channel.", req.Initiator)
			targetMsg := helpers.BuildAtMsg(req.Initiator, req.Target, targetInstr, content)

			// Inject initiator pane
			if err := helpers.SafeInjectText(p, initiatorInfo.TmuxTarget, initiatorMsg); err != nil {
				logger.Info(fmt.Sprintf("@ open existing inject initiator error: %v", err))
			}

			// Inject and TG to target
			if err := helpers.SafeInjectText(p, targetInfo.TmuxTarget, targetMsg); err != nil {
				logger.Info(fmt.Sprintf("@ open inject target error: %v", err))
			}
			if targetChat != nil {
				var sendOpts []interface{}
				if targetTopicID > 0 {
					sendOpts = append(sendOpts, &tele.SendOptions{ThreadID: targetTopicID})
				}
				helpers.RetrySend(bot, targetChat, targetMsg, sendOpts...)
			}
		}

		if isNew {
			logger.Info(fmt.Sprintf("@ channel opened: %s → %s rounds=%d lines=%d isNew=true", req.Initiator, req.Target, req.Rounds, req.Lines))
		} else {
			logger.Info(fmt.Sprintf("@ channel opened: %s → %s isNew=false message=%s", req.Initiator, req.Target, req.Message))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})

	mux.HandleFunc("/at/close", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Initiator string `json:"initiator"`
			Target    string `json:"target"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !bs.AtChannels.Close(req.Initiator, req.Target) {
			http.Error(w, "channel not found", http.StatusNotFound)
			return
		}
		// Both sides receive the same TG message
		notifyMsg := helpers.BuildAtMsg(req.Initiator, req.Target, "", "channel closed")
		p := buildSafeInjectParams(bs)

		for _, name := range []string{req.Initiator, req.Target} {
			info := bs.SessionState.FindByName(name)
			if info == nil {
				continue
			}
			chat, _, topicID := helpers.ResolveChat(bs.SessionState, info.TmuxTarget)
			if chat != nil {
				var sendOpts []interface{}
				if topicID > 0 {
					sendOpts = append(sendOpts, &tele.SendOptions{ThreadID: topicID})
				}
				helpers.RetrySend(bot, chat, notifyMsg, sendOpts...)
			}
			// Only the closed side (req.Target) gets pane inject
			if name == req.Target {
				if err := helpers.SafeInjectText(p, info.TmuxTarget, notifyMsg); err != nil {
					logger.Info(fmt.Sprintf("@ close inject error: %v", err))
				}
			}
		}

		logger.Info(fmt.Sprintf("@ channel closed: %s ↔ %s", req.Initiator, req.Target))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})

	mux.HandleFunc("/at/reply", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			From string `json:"from"` // target (the one replying)
			To   string `json:"to"`   // initiator (the one who opened the channel)
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Validate: from is target, to is initiator — channel must exist
		initiators := bs.AtChannels.GetInitiators(req.From)
		found := false
		for _, init := range initiators {
			if init == req.To {
				found = true
				break
			}
		}
		if !found {
			http.Error(w, "no @ channel from "+req.To+" to "+req.From, http.StatusNotFound)
			return
		}
		toInfo := bs.SessionState.FindByName(req.To)
		if toInfo == nil {
			http.Error(w, "session not found: "+req.To, http.StatusNotFound)
			return
		}

		content := fmt.Sprintf("[%s → %s]: %s", req.From, req.To, req.Text)
		instructions := fmt.Sprintf("`%s` replied via @ channel.", req.From)
		fullMsg := helpers.BuildAtMsg(req.From, req.To, instructions, content)

		// Inject to receiver (req.To) pane
		p := buildSafeInjectParams(bs)
		if err := helpers.SafeInjectText(p, toInfo.TmuxTarget, fullMsg); err != nil {
			logger.Info(fmt.Sprintf("@ reply inject error: %v", err))
		}

		// TG to receiver using SendPagedForward
		chat, _, topicID := helpers.ResolveChat(bs.SessionState, toInfo.TmuxTarget)
		if chat != nil {
			var sendOpts []interface{}
			if topicID > 0 {
				sendOpts = append(sendOpts, &tele.SendOptions{ThreadID: topicID})
			}
			targetHeader := helpers.BuildAtHeader(req.From, req.To) + "\n---\n" + instructions + "\n---\n"
			helpers.SendPagedForward(bot, chat, targetHeader, content, bs.Pages, "", sendOpts...)
		}

		logger.Info(fmt.Sprintf("@ reply: from=%s to=%s text=%s", req.From, req.To, req.Text))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})

	mux.HandleFunc("/at/list", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		w.Header().Set("Content-Type", "application/json")
		if name != "" {
			targets := bs.AtChannels.GetTargets(name)
			initiators := bs.AtChannels.GetInitiators(name)
			// Merge peers (deduplicated)
			peerSet := make(map[string]bool)
			for _, t := range targets {
				peerSet[t] = true
			}
			for _, i := range initiators {
				peerSet[i] = true
			}
			peers := make([]string, 0, len(peerSet))
			for p := range peerSet {
				peers = append(peers, p)
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"name":  name,
				"peers": peers,
			})
			return
		}
		// List all sessions that have at channels
		type atEntry struct {
			Name       string   `json:"name"`
			Targets    []string `json:"targets"`
			Initiators []string `json:"initiators"`
		}
		result := make([]atEntry, 0)
		seen := make(map[string]bool)
		for _, info := range bs.SessionState.All() {
			n := info.Name
			if n == "" || seen[n] {
				continue
			}
			targets := bs.AtChannels.GetTargets(n)
			initiators := bs.AtChannels.GetInitiators(n)
			if len(targets) == 0 && len(initiators) == 0 {
				continue
			}
			seen[n] = true
			result = append(result, atEntry{Name: n, Targets: targets, Initiators: initiators})
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"channels": result})
	})
}
