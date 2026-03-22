package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/logger"
	tele "gopkg.in/telebot.v3"
)

func startCronLoop(ctx context.Context, bot *tele.Bot) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			checkAndRunCronJobs(bot)
		}
	}
}

func checkAndRunCronJobs(bot *tele.Bot) {
	now := time.Now()
	for _, job := range cronJobs.all() {
		if shouldRunCronJob(job, now) {
			go executeCronJob(job, bot, now)
		}
	}
}

func shouldRunCronJob(job *cronJob, now time.Time) bool {
	if dur, err := time.ParseDuration(job.Schedule); err == nil {
		if job.LastRun.IsZero() {
			return now.Sub(job.CreatedAt) >= dur
		}
		return now.Sub(job.LastRun) >= dur
	}
	return matchesCronExpression(job.Schedule, now) && (job.LastRun.IsZero() || now.Sub(job.LastRun) > 55*time.Second)
}

func executeCronJob(job *cronJob, bot *tele.Bot, now time.Time) {
	cronJobs.updateLastRun(job.ID, now)
	logger.Info(fmt.Sprintf("Cron job executing: id=%s mode=%s schedule=%s", job.ID[:8], job.Mode, job.Schedule))
	switch job.Mode {
	case "print":
		executePrintJob(job, bot)
	case "inject":
		executeInjectJob(job, bot)
	}
	if job.Once {
		cronJobs.remove(job.ID)
		logger.Info(fmt.Sprintf("Cron job auto-deleted (once): id=%s", job.ID[:8]))
	}
}

func executePrintJob(job *cronJob, bot *tele.Bot) {
	cfg, _ := config.LoadAppConfig()
	claudeCmd := cfg.ClaudeCommand
	var args []string
	args = append(args, "-p", job.Prompt)
	if job.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(job.MaxTurns))
	}
	if job.SessionID != "" {
		args = append(args, "--resume", job.SessionID, "--output-format", "text")
	} else {
		args = append(args, "--output-format", "json")
	}
	args = append(args, "--dangerously-skip-permissions")
	c := exec.Command(claudeCmd, args...)
	if job.CWD != "" {
		c.Dir = job.CWD
	}
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "CLAUDECODE=") {
			env = append(env, e)
		}
	}
	env = append(env, "TG_CLI_CRON=1")
	c.Env = env
	output, err := c.CombinedOutput()
	if err != nil {
		logger.Error(fmt.Sprintf("Cron print job failed: id=%s err=%v output=%s", job.ID[:8], err, truncateStr(string(output), 500)))
		sendCronNotification(bot, fmt.Sprintf("%s\n\n❌ <b>Error:</b> %s", cronNotifyHeader(job), err.Error()), "")
		return
	}
	var result string
	if job.SessionID == "" {
		sid, res := parsePrintJobOutput(output)
		if sid != "" {
			cronJobs.updateSessionID(job.ID, sid)
		}
		result = res
	} else {
		result = strings.TrimSpace(string(output))
	}
	logger.Info(fmt.Sprintf("Cron print job result: id=%s result=%s", job.ID[:8], truncateStr(result, 200)))
	if strings.HasPrefix(strings.TrimSpace(result), "HEARTBEAT_OK") {
		logger.Info(fmt.Sprintf("Cron heartbeat OK: id=%s", job.ID[:8]))
		return
	}
	sendCronNotification(bot, fmt.Sprintf("%s\n\n%s", cronNotifyHeader(job), result), "")
}

func parsePrintJobOutput(output []byte) (sessionID, result string) {
	var entries []json.RawMessage
	if err := json.Unmarshal(output, &entries); err != nil {
		return "", strings.TrimSpace(string(output))
	}
	for _, entry := range entries {
		var obj struct {
			Type      string `json:"type"`
			SessionID string `json:"session_id"`
			Result    string `json:"result"`
		}
		if err := json.Unmarshal(entry, &obj); err != nil {
			continue
		}
		if obj.Type == "result" {
			return obj.SessionID, obj.Result
		}
	}
	return "", strings.TrimSpace(string(output))
}

func executeInjectJob(job *cronJob, bot *tele.Bot) {
	// Try agent name first, then tmux target fallback
	var info *sessionInfo
	if job.AgentName != "" {
		info = sessionState.findByName(job.AgentName)
	}
	if info == nil && job.TmuxTarget != "" {
		info = sessionState.findInfoByTarget(job.TmuxTarget)
	}
	agentLabel := job.AgentName
	if agentLabel == "" {
		agentLabel = job.TmuxTarget
	}
	if info == nil {
		logger.Info(fmt.Sprintf("Cron inject job: agent '%s' not online, sending TG notification", agentLabel))
		sendCronNotification(bot, fmt.Sprintf("%s\n\n⚠️ Agent <b>%s</b> is not online.\nPrompt: %s", cronNotifyHeader(job), agentLabel, job.Prompt), job.TmuxTarget)
		return
	}
	injectText := job.Prompt
	if !job.NoHeader {
		headerName := job.Name
		if headerName == "" {
			headerName = job.ID[:8]
		}
		injectText = fmt.Sprintf("---\n⏰ Cron: %s\n---\n%s", headerName, job.Prompt)
	}
	if err := safeInjectText(bot, info.tmuxTarget, injectText); err != nil {
		logger.Error(fmt.Sprintf("Cron inject job: inject failed: %v", err))
		sendCronNotification(bot, fmt.Sprintf("%s\n\n❌ <b>Inject failed</b>\nAgent: %s\nError: %s", cronNotifyHeader(job), agentLabel, err.Error()), info.tmuxTarget)
		return
	}
	logger.Info(fmt.Sprintf("Cron inject job: injected to '%s' target=%s text=%s", agentLabel, info.tmuxTarget, truncateStr(job.Prompt, 200)))
	sendCronNotification(bot, fmt.Sprintf("%s\n\n✅ Injected → %s\n\n%s", cronNotifyHeader(job), agentLabel, job.Prompt), info.tmuxTarget)
}

func cronNotifyHeader(job *cronJob) string {
	icon := "🔔"
	label := "Cron"
	if job.NoHeader {
		icon = "📨"
		label = "Cron (silent)"
	}
	header := fmt.Sprintf("%s <b>%s</b> <code>%s</code>", icon, label, job.ID[:8])
	if job.Name != "" {
		header += fmt.Sprintf(" (<b>%s</b>)", job.Name)
	}
	if job.CWD != "" {
		header += fmt.Sprintf("\n📂 %s", job.CWD)
	}
	return header
}

func sendCronNotification(bot *tele.Bot, text string, tmuxTarget string) {
	chat, _, topicID := resolveChat(tmuxTarget)
	if chat == nil {
		return
	}
	var sendOpts []interface{}
	sendOpts = append(sendOpts, tele.ModeHTML)
	if topicID > 0 {
		sendOpts = append(sendOpts, &tele.SendOptions{ThreadID: topicID})
	}
	chunks := splitBody(text, 3900)
	if len(chunks) <= 1 {
		retrySend(bot, chat, text, sendOpts...)
		return
	}
	firstText := chunks[0] + fmt.Sprintf("\n\n📄 1/%d", len(chunks))
	kb := buildPageKeyboard(1, len(chunks))
	opts := append([]interface{}{kb}, sendOpts...)
	sent, err := retrySend(bot, chat, firstText, opts...)
	if err != nil {
		return
	}
	pages.store(sent.ID, "", &pageEntry{chunks: chunks, chatID: chat.ID})
}

func matchesCronExpression(expr string, t time.Time) bool {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return false
	}
	return matchCronField(fields[0], t.Minute(), 0, 59) &&
		matchCronField(fields[1], t.Hour(), 0, 23) &&
		matchCronField(fields[2], t.Day(), 1, 31) &&
		matchCronField(fields[3], int(t.Month()), 1, 12) &&
		matchCronField(fields[4], int(t.Weekday()), 0, 6)
}

func matchCronField(field string, value, min, max int) bool {
	if field == "*" {
		return true
	}
	if strings.Contains(field, "/") {
		parts := strings.SplitN(field, "/", 2)
		step, err := strconv.Atoi(parts[1])
		if err != nil || step <= 0 {
			return false
		}
		if parts[0] == "*" {
			return value%step == 0
		}
		rangeParts := strings.SplitN(parts[0], "-", 2)
		if len(rangeParts) == 2 {
			lo, _ := strconv.Atoi(rangeParts[0])
			hi, _ := strconv.Atoi(rangeParts[1])
			if value < lo || value > hi {
				return false
			}
			return (value-lo)%step == 0
		}
		return false
	}
	if strings.Contains(field, ",") {
		for _, part := range strings.Split(field, ",") {
			if matchCronField(strings.TrimSpace(part), value, min, max) {
				return true
			}
		}
		return false
	}
	if strings.Contains(field, "-") {
		parts := strings.SplitN(field, "-", 2)
		lo, _ := strconv.Atoi(parts[0])
		hi, _ := strconv.Atoi(parts[1])
		return value >= lo && value <= hi
	}
	n, err := strconv.Atoi(field)
	if err != nil {
		return false
	}
	return value == n
}

func generateCronID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}
