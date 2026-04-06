package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Seraphli/tg-cli/cmd/helpers"
	"github.com/Seraphli/tg-cli/cmd/types"
	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/logger"
	"github.com/Seraphli/tg-cli/internal/markdown"
	tele "gopkg.in/telebot.v3"
)

// LaunchState holds the state for the /bot_new interactive launch flow.
type LaunchState struct {
	Step        string `json:"step"`
	SessionName string `json:"sessionName"`
	WorkDir     string `json:"workDir"`
	Command     string `json:"command"`
	AgentName   string `json:"agentName,omitempty"`
	MsgID       int    `json:"msgID"`
	ChatID      int64  `json:"chatID"`
	TopicID     int    `json:"topicID,omitempty"`
	UUID        string `json:"uuid"`
	BrowsePath  string `json:"browsePath,omitempty"`
	ShowHidden  bool   `json:"showHidden,omitempty"`
	DirPage     int    `json:"dirPage,omitempty"`
}

func launchDir() string {
	base := filepath.Base(config.GetConfigDir())
	dir := filepath.Join("/tmp", base, "launch")
	os.MkdirAll(dir, 0755)
	return dir
}

func writeLaunchState(state *LaunchState) error {
	path := filepath.Join(launchDir(), state.UUID+".json")
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// DeleteLaunchState removes the persisted launch state file for the given UUID.
func DeleteLaunchState(uuid string) {
	os.Remove(filepath.Join(launchDir(), uuid+".json"))
}

// GenerateLaunchUUID generates a random UUID for a new launch flow.
func GenerateLaunchUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ParseBotNewArgs parses /bot_new arguments.
func ParseBotNewArgs(text string) (sessionName, workDir, command string) {
	words := strings.Fields(text)
	var remaining []string
	for _, w := range words {
		if strings.HasPrefix(w, "@") && sessionName == "" {
			sessionName = w[1:]
		} else if strings.HasPrefix(w, "#") && workDir == "" {
			workDir = strings.Replace(w[1:], "~", os.Getenv("HOME"), 1)
		} else {
			remaining = append(remaining, w)
		}
	}
	command = strings.Join(remaining, " ")
	return
}

// StartLaunchFlow initiates the interactive /bot_new confirmation flow.
func StartLaunchFlow(bs *types.BotState, c tele.Context, sessionName, workDir, command string) error {
	state := &LaunchState{
		SessionName: sessionName,
		WorkDir:     workDir,
		Command:     command,
		ChatID:      c.Chat().ID,
		TopicID:     c.Message().ThreadID,
		UUID:        GenerateLaunchUUID(),
	}
	if sessionName == "" {
		AskSessionName(bs, c.Bot(), c.Chat().ID, state)
	} else if workDir == "" {
		AskWorkDir(bs, c.Bot(), c.Chat().ID, state)
	} else {
		go ExecuteLaunch(bs, c.Bot(), c.Chat().ID, state)
	}
	return nil
}

func launchSendOpts(state *LaunchState, markup ...*tele.ReplyMarkup) []interface{} {
	if state.TopicID > 0 {
		so := &tele.SendOptions{ThreadID: state.TopicID}
		if len(markup) > 0 {
			so.ReplyMarkup = markup[0]
		}
		return []interface{}{so, tele.ModeHTML}
	}
	if len(markup) > 0 {
		return []interface{}{markup[0], tele.ModeHTML}
	}
	return []interface{}{tele.ModeHTML}
}

// AskSessionName sends a TG message asking the user for a session name.
func AskSessionName(bs *types.BotState, bot *tele.Bot, chatID int64, state *LaunchState) {
	cfg, err := config.LoadAppConfig()
	if err != nil {
		logger.Error(fmt.Sprintf("askSessionName: failed to load config: %v", err))
		return
	}
	sel := &tele.ReplyMarkup{}
	btnDefault := sel.Data(fmt.Sprintf("Use default: %s", cfg.DefaultSessionName), "bot_new", "session_default")
	btnCancel := sel.Data("❌ Cancel", "bot_new", "cancel")
	sel.Inline(sel.Row(btnDefault, btnCancel))
	sent, err := helpers.RetrySend(bot, &tele.Chat{ID: chatID}, fmt.Sprintf("📦 Session name\nDefault: %s\n\n💡 Click the button to use default, or reply to this message with a custom name.", markdown.EscapeHTML(cfg.DefaultSessionName)), launchSendOpts(state, sel)...)
	if err != nil {
		logger.Error(fmt.Sprintf("askSessionName: failed to send: %v", err))
		return
	}
	state.MsgID = sent.ID
	state.Step = "session"
	state.ChatID = chatID
	if err := writeLaunchState(state); err != nil {
		logger.Error(fmt.Sprintf("askSessionName: failed to write pending state: %v", err))
	}
	bs.LaunchPending.Store(sent.ID, state)
	logger.Info(fmt.Sprintf("askSessionName: sent msg_id=%d uuid=%s buttons=1", sent.ID, state.UUID))
}

// AskWorkDir sends a TG directory browser message for the user to pick a working directory.
func AskWorkDir(bs *types.BotState, bot *tele.Bot, chatID int64, state *LaunchState) {
	if state.BrowsePath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			logger.Error(fmt.Sprintf("askWorkDir: failed to get home dir: %v", err))
			return
		}
		state.BrowsePath = home
	}
	state.DirPage = 0
	dirs, err := ListSubDirs(state.BrowsePath, state.ShowHidden)
	if err != nil {
		logger.Error(fmt.Sprintf("askWorkDir: failed to list dirs %s: %v", state.BrowsePath, err))
		dirs = []string{}
	}
	kb := buildDirKeyboard(dirs, 0, state.ShowHidden)
	text := fmt.Sprintf("📂 Select working directory\nCurrent: %s\n(%d subdirectories)\n\n💡 Click 📁 to enter a folder, ✅ to select current directory, or reply with an absolute path.", markdown.EscapeHTML(state.BrowsePath), len(dirs))
	sent, err := helpers.RetrySend(bot, &tele.Chat{ID: chatID}, text, launchSendOpts(state, kb)...)
	if err != nil {
		logger.Error(fmt.Sprintf("askWorkDir: failed to send: %v", err))
		return
	}
	state.MsgID = sent.ID
	state.Step = "workdir"
	state.ChatID = chatID
	if err := writeLaunchState(state); err != nil {
		logger.Error(fmt.Sprintf("askWorkDir: failed to write pending state: %v", err))
	}
	bs.LaunchPending.Store(sent.ID, state)
	logger.Info(fmt.Sprintf("askWorkDir: sent msg_id=%d uuid=%s browsePath=%s buttons=%d", sent.ID, state.UUID, state.BrowsePath, len(kb.InlineKeyboard)))
}

// ExecuteLaunch creates (or reuses) a tmux session/window and runs the command.
func ExecuteLaunch(bs *types.BotState, bot *tele.Bot, chatID int64, state *LaunchState) {
	cfg, err := config.LoadAppConfig()
	if err != nil {
		logger.Error(fmt.Sprintf("executeLaunch: failed to load config: %v", err))
		helpers.RetrySend(bot, &tele.Chat{ID: chatID}, fmt.Sprintf("❌ Failed to load config: %s", markdown.EscapeHTML(err.Error())), launchSendOpts(state)...)
		return
	}
	if state.SessionName == "" {
		state.SessionName = cfg.DefaultSessionName
	}
	if state.WorkDir == "" {
		state.WorkDir = cfg.DefaultWorkDir
	}
	if strings.HasPrefix(state.WorkDir, "~") {
		home, err := os.UserHomeDir()
		if err == nil {
			state.WorkDir = home + state.WorkDir[1:]
		}
	}
	state.Step = "launching"
	if err := writeLaunchState(state); err != nil {
		logger.Error(fmt.Sprintf("executeLaunch: failed to write launch state: %v", err))
	}
	var paneID string
	if injector.NamedSessionExists(state.SessionName) {
		helpers.RetrySend(bot, &tele.Chat{ID: chatID}, fmt.Sprintf("ℹ️ Session '%s' already exists, creating a new window in it.", markdown.EscapeHTML(state.SessionName)), launchSendOpts(state)...)
		id, err := injector.CreateWindow(state.SessionName, state.WorkDir)
		if err != nil {
			logger.Error(fmt.Sprintf("executeLaunch: CreateWindow failed: session=%s err=%v", state.SessionName, err))
			helpers.RetrySend(bot, &tele.Chat{ID: chatID}, fmt.Sprintf("❌ Failed to create window in session %s: %s", markdown.EscapeHTML(state.SessionName), markdown.EscapeHTML(err.Error())), launchSendOpts(state)...)
			DeleteLaunchState(state.UUID)
			return
		}
		paneID = id
		logger.Info(fmt.Sprintf("executeLaunch: created window in session=%s pane=%s", state.SessionName, paneID))
	} else {
		if err := injector.CreateSession(state.SessionName, state.WorkDir); err != nil {
			logger.Error(fmt.Sprintf("executeLaunch: CreateSession failed: session=%s err=%v", state.SessionName, err))
			helpers.RetrySend(bot, &tele.Chat{ID: chatID}, fmt.Sprintf("❌ Failed to create session %s: %s", markdown.EscapeHTML(state.SessionName), markdown.EscapeHTML(err.Error())), launchSendOpts(state)...)
			DeleteLaunchState(state.UUID)
			return
		}
		panes, err := injector.ListPanes(state.SessionName)
		if err != nil || len(panes) == 0 {
			logger.Error(fmt.Sprintf("executeLaunch: ListPanes failed: session=%s err=%v", state.SessionName, err))
			helpers.RetrySend(bot, &tele.Chat{ID: chatID}, fmt.Sprintf("❌ Failed to list panes in session %s: %s", markdown.EscapeHTML(state.SessionName), markdown.EscapeHTML(err.Error())), launchSendOpts(state)...)
			DeleteLaunchState(state.UUID)
			return
		}
		paneID = panes[0]
		logger.Info(fmt.Sprintf("executeLaunch: created session=%s pane=%s", state.SessionName, paneID))
	}
	target := injector.TmuxTarget{PaneID: paneID}
	cmd := state.Command
	if cmd == "" {
		cmd = cfg.ClaudeCommand
	}
	if err := injector.InjectText(target, cmd); err != nil {
		logger.Error(fmt.Sprintf("executeLaunch: InjectText failed: pane=%s cmd=%s err=%v", paneID, cmd, err))
		helpers.RetrySend(bot, &tele.Chat{ID: chatID}, fmt.Sprintf("❌ Failed to inject command to pane %s: %s", markdown.EscapeHTML(paneID), markdown.EscapeHTML(err.Error())), launchSendOpts(state)...)
		DeleteLaunchState(state.UUID)
		return
	}
	go func() {
		time.Sleep(5 * time.Second)
		content, err := injector.CapturePane(target)
		if err != nil {
			logger.Error(fmt.Sprintf("executeLaunch: CapturePane failed: pane=%s err=%v", paneID, err))
			return
		}
		lower := strings.ToLower(content)
		if strings.Contains(lower, "trust") {
			logger.Info(fmt.Sprintf("executeLaunch: trust dialog detected, sending Enter: pane=%s", paneID))
			injector.SendKeys(target, "Enter")
		} else if strings.Contains(lower, "yes, continue") || strings.Contains(lower, "continue?") {
			logger.Info(fmt.Sprintf("executeLaunch: Codex continue dialog detected, sending Enter: pane=%s", paneID))
			injector.SendKeys(target, "Enter")
		}
	}()
	msg := fmt.Sprintf("🚀 CC launched\n📦 %s\n📟 %s\n📂 %s\n💻 %s", markdown.EscapeHTML(state.SessionName), markdown.EscapeHTML(paneID), markdown.EscapeHTML(state.WorkDir), markdown.EscapeHTML(cmd))
	helpers.RetrySend(bot, &tele.Chat{ID: chatID}, msg, launchSendOpts(state)...)
	logger.Info(fmt.Sprintf("executeLaunch: done session=%s pane=%s workdir=%s cmd=%s", state.SessionName, paneID, state.WorkDir, cmd))
	if state.AgentName != "" {
		agentName := state.AgentName
		pane := paneID
		go func() {
			for i := 0; i < 30; i++ {
				time.Sleep(1 * time.Second)
				sid, found := bs.SessionState.FindByTarget(pane)
				if found {
					bs.SessionState.SetName(sid, agentName)
					logger.Info(fmt.Sprintf("executeLaunch: agent name auto-set: session=%s name=%s", sid, agentName))
					return
				}
			}
			logger.Error(fmt.Sprintf("executeLaunch: failed to auto-set agent name: pane=%s name=%s (timeout)", pane, agentName))
		}()
	}
	state.Step = "done"
	DeleteLaunchState(state.UUID)
	bs.LaunchPending.Delete(state.MsgID)
}

// ResumeLaunchFlow restores a launch flow after a crash/restart.
func ResumeLaunchFlow(bs *types.BotState, state *LaunchState) {
	bot := bs.Bot
	switch state.Step {
	case "session":
		AskSessionName(bs, bot, state.ChatID, state)
	case "workdir":
		AskWorkDir(bs, bot, state.ChatID, state)
	case "launching":
		go ExecuteLaunch(bs, bot, state.ChatID, state)
	case "done":
		DeleteLaunchState(state.UUID)
	default:
		logger.Error(fmt.Sprintf("resumeLaunchFlow: unknown step=%s uuid=%s", state.Step, state.UUID))
		DeleteLaunchState(state.UUID)
	}
}

// ScanLaunchDir scans pending launch states on bot startup and resumes them.
func ScanLaunchDir(bs *types.BotState) {
	dir := launchDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if time.Since(info.ModTime()) > 10*time.Minute {
			logger.Info(fmt.Sprintf("scanLaunchDir: expired launch state %s (age=%v), deleting", entry.Name(), time.Since(info.ModTime()).Truncate(time.Second)))
			os.Remove(path)
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var state LaunchState
		if err := json.Unmarshal(data, &state); err != nil {
			logger.Error(fmt.Sprintf("scanLaunchDir: failed to parse %s: %v", entry.Name(), err))
			continue
		}
		logger.Info(fmt.Sprintf("scanLaunchDir: resuming launch uuid=%s step=%s", state.UUID, state.Step))
		ResumeLaunchFlow(bs, &state)
	}
}

// ListSubDirs returns a sorted list of subdirectory names in dirPath, optionally including hidden ones.
func ListSubDirs(dirPath string, showHidden bool) ([]string, error) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !showHidden && strings.HasPrefix(name, ".") {
			continue
		}
		dirs = append(dirs, name)
	}
	sort.Slice(dirs, func(i, j int) bool { return strings.ToLower(dirs[i]) < strings.ToLower(dirs[j]) })
	return dirs, nil
}

const dirsPerPage = 16

func buildDirKeyboard(dirs []string, page int, showHidden bool) *tele.ReplyMarkup {
	sel := &tele.ReplyMarkup{}
	var rows []tele.Row
	totalPages := (len(dirs) + dirsPerPage - 1) / dirsPerPage
	if totalPages == 0 {
		totalPages = 1
	}
	if page >= totalPages {
		page = totalPages - 1
	}
	start := page * dirsPerPage
	end := start + dirsPerPage
	if end > len(dirs) {
		end = len(dirs)
	}
	pageDirs := dirs[start:end]
	for i := 0; i < len(pageDirs); i += 2 {
		btn1 := sel.Data("📁 "+pageDirs[i], "bot_new", fmt.Sprintf("cd:%d", start+i))
		if i+1 < len(pageDirs) {
			btn2 := sel.Data("📁 "+pageDirs[i+1], "bot_new", fmt.Sprintf("cd:%d", start+i+1))
			rows = append(rows, sel.Row(btn1, btn2))
		} else {
			rows = append(rows, sel.Row(btn1))
		}
	}
	if totalPages > 1 {
		var pageBtns []tele.Btn
		if page > 0 {
			pageBtns = append(pageBtns, sel.Data("⬅️", "bot_new", "page_prev"))
		}
		pageBtns = append(pageBtns, sel.Data(fmt.Sprintf("%d/%d", page+1, totalPages), "bot_new", "page_noop"))
		if page < totalPages-1 {
			pageBtns = append(pageBtns, sel.Data("➡️", "bot_new", "page_next"))
		}
		rows = append(rows, tele.Row(pageBtns))
	}
	hiddenLabel := "👁 Show hidden"
	if showHidden {
		hiddenLabel = "🙈 Hide hidden"
	}
	btnUp := sel.Data("📁 ..", "bot_new", "cd_up")
	btnHidden := sel.Data(hiddenLabel, "bot_new", "toggle_hidden")
	rows = append(rows, sel.Row(btnUp, btnHidden))
	btnSelect := sel.Data("✅ Select this directory", "bot_new", "dir_select")
	btnCancel := sel.Data("❌ Cancel", "bot_new", "cancel")
	rows = append(rows, sel.Row(btnSelect, btnCancel))
	sel.Inline(rows...)
	return sel
}

// RefreshDirBrowser edits the existing TG message to show updated directory listing.
func RefreshDirBrowser(bot *tele.Bot, msg *tele.Message, state *LaunchState) {
	dirs, err := ListSubDirs(state.BrowsePath, state.ShowHidden)
	if err != nil {
		dirs = []string{}
	}
	kb := buildDirKeyboard(dirs, state.DirPage, state.ShowHidden)
	text := fmt.Sprintf("📂 Select working directory\nCurrent: %s\n(%d subdirectories)\n\n💡 Click 📁 to enter a folder, ✅ to select current directory, or reply with an absolute path.", markdown.EscapeHTML(state.BrowsePath), len(dirs))
	helpers.RetryEdit(bot, msg, text, kb, tele.ModeHTML)
	writeLaunchState(state)
}
