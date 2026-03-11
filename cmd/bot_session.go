package cmd

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/logger"
	tele "gopkg.in/telebot.v3"
)

// LaunchState holds the state for the /bot_new interactive launch flow.
// Persisted to a pending file for crash recovery.
type LaunchState struct {
	Step        string `json:"step"`        // "session", "workdir", "launching", "done"
	SessionName string `json:"sessionName"`
	WorkDir     string `json:"workDir"`
	Command     string `json:"command"`
	MsgID       int    `json:"msgID"`
	ChatID      int64  `json:"chatID"`
	UUID        string `json:"uuid"`
	BrowsePath  string `json:"browsePath,omitempty"`
	ShowHidden  bool   `json:"showHidden,omitempty"`
	DirPage     int    `json:"dirPage,omitempty"`
}

// launchPending maps msgID (int) -> *LaunchState for callback/reply routing
var launchPending sync.Map

// launchDir returns the directory used to persist LaunchState files
func launchDir() string {
	base := filepath.Base(config.GetConfigDir())
	dir := filepath.Join("/tmp", base, "launch")
	os.MkdirAll(dir, 0755)
	return dir
}

// writeLaunchState atomically writes a LaunchState to disk
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

// deleteLaunchState removes the persisted launch state file
func deleteLaunchState(uuid string) {
	os.Remove(filepath.Join(launchDir(), uuid+".json"))
}

// generateLaunchUUID generates a random 16-byte hex UUID
func generateLaunchUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// parseBotNewArgs parses /bot_new arguments.
// @xxx sets session name, #xxx sets workdir, remaining words form the command.
func parseBotNewArgs(text string) (sessionName, workDir, command string) {
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

// startLaunchFlow initiates the interactive /bot_new confirmation flow.
func startLaunchFlow(c tele.Context, sessionName, workDir, command string) error {
	state := &LaunchState{
		SessionName: sessionName,
		WorkDir:     workDir,
		Command:     command,
		ChatID:      c.Chat().ID,
		UUID:        generateLaunchUUID(),
	}
	if sessionName == "" {
		askSessionName(c.Bot(), c.Chat().ID, state)
	} else if workDir == "" {
		askWorkDir(c.Bot(), c.Chat().ID, state)
	} else {
		go executeLaunch(c.Bot(), c.Chat().ID, state)
	}
	return nil
}

// askSessionName sends a TG message asking the user for a session name.
func askSessionName(bot *tele.Bot, chatID int64, state *LaunchState) {
	cfg, err := config.LoadAppConfig()
	if err != nil {
		logger.Error(fmt.Sprintf("askSessionName: failed to load config: %v", err))
		return
	}
	sel := &tele.ReplyMarkup{}
	btnDefault := sel.Data(fmt.Sprintf("Use default: %s", cfg.DefaultSessionName), "bot_new", "session_default")
	btnCancel := sel.Data("❌ Cancel", "bot_new", "cancel")
	sel.Inline(sel.Row(btnDefault, btnCancel))
	sent, err := retrySend(bot, &tele.Chat{ID: chatID}, fmt.Sprintf("📦 Session name\nDefault: %s\n\n💡 Click the button to use default, or reply to this message with a custom name.", cfg.DefaultSessionName), sel)
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
	launchPending.Store(sent.ID, state)
	logger.Info(fmt.Sprintf("askSessionName: sent msg_id=%d uuid=%s buttons=1", sent.ID, state.UUID))
}

// askWorkDir sends a TG directory browser message for the user to pick a working directory.
func askWorkDir(bot *tele.Bot, chatID int64, state *LaunchState) {
	if state.BrowsePath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			logger.Error(fmt.Sprintf("askWorkDir: failed to get home dir: %v", err))
			return
		}
		state.BrowsePath = home
	}
	state.DirPage = 0
	dirs, err := listSubDirs(state.BrowsePath, state.ShowHidden)
	if err != nil {
		logger.Error(fmt.Sprintf("askWorkDir: failed to list dirs %s: %v", state.BrowsePath, err))
		dirs = []string{}
	}
	kb := buildDirKeyboard(dirs, 0, state.ShowHidden)
	text := fmt.Sprintf("📂 Select working directory\nCurrent: %s\n(%d subdirectories)\n\n💡 Click 📁 to enter a folder, ✅ to select current directory, or reply with an absolute path.", state.BrowsePath, len(dirs))
	sent, err := retrySend(bot, &tele.Chat{ID: chatID}, text, kb)
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
	launchPending.Store(sent.ID, state)
	logger.Info(fmt.Sprintf("askWorkDir: sent msg_id=%d uuid=%s browsePath=%s buttons=%d", sent.ID, state.UUID, state.BrowsePath, len(kb.InlineKeyboard)))
}

// executeLaunch creates (or reuses) a tmux session/window and runs the command.
func executeLaunch(bot *tele.Bot, chatID int64, state *LaunchState) {
	cfg, err := config.LoadAppConfig()
	if err != nil {
		logger.Error(fmt.Sprintf("executeLaunch: failed to load config: %v", err))
		retrySend(bot, &tele.Chat{ID: chatID}, fmt.Sprintf("❌ Failed to load config: %v", err))
		return
	}
	// Fill defaults for any still-empty fields
	if state.SessionName == "" {
		state.SessionName = cfg.DefaultSessionName
	}
	if state.WorkDir == "" {
		state.WorkDir = cfg.DefaultWorkDir
	}
	// Expand leading ~ in workDir
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
		// Session exists — create a new window
		retrySend(bot, &tele.Chat{ID: chatID}, fmt.Sprintf("ℹ️ Session '%s' already exists, creating a new window in it.", state.SessionName))
		id, err := injector.CreateWindow(state.SessionName, state.WorkDir)
		if err != nil {
			logger.Error(fmt.Sprintf("executeLaunch: CreateWindow failed: session=%s err=%v", state.SessionName, err))
			retrySend(bot, &tele.Chat{ID: chatID}, fmt.Sprintf("❌ Failed to create window in session %s: %v", state.SessionName, err))
			deleteLaunchState(state.UUID)
			return
		}
		paneID = id
		logger.Info(fmt.Sprintf("executeLaunch: created window in session=%s pane=%s", state.SessionName, paneID))
	} else {
		// Session does not exist — create a new session
		if err := injector.CreateSession(state.SessionName, state.WorkDir); err != nil {
			logger.Error(fmt.Sprintf("executeLaunch: CreateSession failed: session=%s err=%v", state.SessionName, err))
			retrySend(bot, &tele.Chat{ID: chatID}, fmt.Sprintf("❌ Failed to create session %s: %v", state.SessionName, err))
			deleteLaunchState(state.UUID)
			return
		}
		panes, err := injector.ListPanes(state.SessionName)
		if err != nil || len(panes) == 0 {
			logger.Error(fmt.Sprintf("executeLaunch: ListPanes failed: session=%s err=%v", state.SessionName, err))
			retrySend(bot, &tele.Chat{ID: chatID}, fmt.Sprintf("❌ Failed to list panes in session %s: %v", state.SessionName, err))
			deleteLaunchState(state.UUID)
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
	// Type command into the pane using set-buffer + paste + Enter
	if err := injector.InjectText(target, cmd); err != nil {
		logger.Error(fmt.Sprintf("executeLaunch: InjectText failed: pane=%s cmd=%s err=%v", paneID, cmd, err))
		retrySend(bot, &tele.Chat{ID: chatID}, fmt.Sprintf("❌ Failed to inject command to pane %s: %v", paneID, err))
		deleteLaunchState(state.UUID)
		return
	}
	// Wait for CC TUI to load, then press Enter if trust dialog appears
	go func() {
		time.Sleep(5 * time.Second)
		content, err := injector.CapturePane(target)
		if err != nil {
			logger.Error(fmt.Sprintf("executeLaunch: CapturePane failed: pane=%s err=%v", paneID, err))
			return
		}
		if strings.Contains(strings.ToLower(content), "trust") {
			logger.Info(fmt.Sprintf("executeLaunch: trust dialog detected, sending Enter: pane=%s", paneID))
			injector.SendKeys(target, "Enter")
		}
	}()

	msg := fmt.Sprintf("🚀 CC launched\n📦 %s\n📟 %s\n📂 %s\n💻 %s", state.SessionName, paneID, state.WorkDir, cmd)
	retrySend(bot, &tele.Chat{ID: chatID}, msg)
	logger.Info(fmt.Sprintf("executeLaunch: done session=%s pane=%s workdir=%s cmd=%s", state.SessionName, paneID, state.WorkDir, cmd))

	state.Step = "done"
	deleteLaunchState(state.UUID)
	launchPending.Delete(state.MsgID)
}

// resumeLaunchFlow restores a launch flow after a crash/restart.
func resumeLaunchFlow(bot *tele.Bot, state *LaunchState) {
	switch state.Step {
	case "session":
		askSessionName(bot, state.ChatID, state)
	case "workdir":
		askWorkDir(bot, state.ChatID, state)
	case "launching":
		go executeLaunch(bot, state.ChatID, state)
	case "done":
		deleteLaunchState(state.UUID)
	default:
		logger.Error(fmt.Sprintf("resumeLaunchFlow: unknown step=%s uuid=%s", state.Step, state.UUID))
		deleteLaunchState(state.UUID)
	}
}

// scanLaunchDir scans pending launch states on bot startup and resumes them.
func scanLaunchDir(bot *tele.Bot) {
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
		resumeLaunchFlow(bot, &state)
	}
}

// listSubDirs returns sorted subdirectory names under dirPath.
func listSubDirs(dirPath string, showHidden bool) ([]string, error) {
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

// buildDirKeyboard builds a paged inline keyboard for directory browsing.
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

// refreshDirBrowser edits the existing TG message to show updated directory listing.
func refreshDirBrowser(bot *tele.Bot, msg *tele.Message, state *LaunchState) {
	dirs, err := listSubDirs(state.BrowsePath, state.ShowHidden)
	if err != nil {
		dirs = []string{}
	}
	kb := buildDirKeyboard(dirs, state.DirPage, state.ShowHidden)
	text := fmt.Sprintf("📂 Select working directory\nCurrent: %s\n(%d subdirectories)\n\n💡 Click 📁 to enter a folder, ✅ to select current directory, or reply with an absolute path.", state.BrowsePath, len(dirs))
	retryEdit(bot, msg, text, kb)
	writeLaunchState(state)
}
