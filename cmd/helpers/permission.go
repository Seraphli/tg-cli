package helpers

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/logger"
)

// PendingFile represents a pending CC event stored as a file.
type PendingFile struct {
	UUID       string          `json:"uuid"`
	Event      string          `json:"event"`
	ToolName   string          `json:"tool_name"`
	Status     string          `json:"status"`
	Payload    json.RawMessage `json:"payload"`
	TgMsgID    int             `json:"tg_msg_id"`
	TgChatID   int64           `json:"tg_chat_id"`
	SessionID  string          `json:"session_id"`
	TmuxTarget string          `json:"tmux_target"`
	CCOutput   json.RawMessage `json:"cc_output"`
	CreatedAt  string          `json:"created_at"`
	HookPID    int             `json:"hook_pid"`
	TgMsgText  string          `json:"tg_msg_text"`
}

// PendingDir returns /tmp/<config-dir-basename>/pending, creating it if needed.
func PendingDir() string {
	base := filepath.Base(config.GetConfigDir())
	dir := filepath.Join("/tmp", base, "pending")
	os.MkdirAll(dir, 0755)
	return dir
}

// ReadPendingFile reads and unmarshals a pending file.
func ReadPendingFile(path string) (*PendingFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var pf PendingFile
	if err := json.Unmarshal(data, &pf); err != nil {
		return nil, err
	}
	return &pf, nil
}

// WritePendingFile atomically writes a pending file.
func WritePendingFile(path string, pf *PendingFile) error {
	data, err := json.MarshalIndent(pf, "", "  ")
	if err != nil {
		return err
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// WritePendingAnswer updates pending file with answer and status=answered.
func WritePendingAnswer(uuid string, ccOutput json.RawMessage) error {
	path := filepath.Join(PendingDir(), uuid+".json")
	pf, err := ReadPendingFile(path)
	if err != nil {
		return fmt.Errorf("read pending file: %w", err)
	}
	pf.Status = "answered"
	pf.CCOutput = ccOutput
	return WritePendingFile(path, pf)
}

// IsHookAlive checks if the hook process with given PID is still running.
func IsHookAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// BuildAskCCOutput builds CC output for AskUserQuestion.
func BuildAskCCOutput(payload json.RawMessage, answers map[string]string) json.RawMessage {
	var p map[string]interface{}
	json.Unmarshal(payload, &p)
	toolInput, _ := p["tool_input"].(map[string]interface{})
	questions, _ := toolInput["questions"].([]interface{})
	output := map[string]interface{}{
		"hookSpecificOutput": map[string]interface{}{
			"hookEventName": "PermissionRequest",
			"decision": map[string]interface{}{
				"behavior": "allow",
				"updatedInput": map[string]interface{}{
					"questions": questions,
					"answers":   answers,
				},
			},
		},
	}
	result, _ := json.Marshal(output)
	return result
}

// BuildPermCCOutput builds CC output for PermissionRequest.
func BuildPermCCOutput(decision string, message string, updatedPerms []interface{}) json.RawMessage {
	output := map[string]interface{}{
		"hookSpecificOutput": map[string]interface{}{
			"hookEventName": "PermissionRequest",
			"decision": map[string]interface{}{
				"behavior": decision,
			},
		},
	}
	decisionMap := output["hookSpecificOutput"].(map[string]interface{})["decision"].(map[string]interface{})
	if message != "" {
		decisionMap["message"] = message
	}
	if updatedPerms != nil {
		decisionMap["updatedPermissions"] = updatedPerms
	}
	result, _ := json.Marshal(output)
	return result
}

// ResolvePermission resolves a PermissionRequest decision.
func ResolvePermission(pendingPerms *stores.PendingPermStore, msgID int, decision string, suggestionsOverride json.RawMessage) (stores.PermDecision, error) {
	d := stores.PermDecision{}
	suggestions := suggestionsOverride
	if suggestions == nil {
		suggestions = pendingPerms.GetSuggestions(msgID)
	}
	switch {
	case decision == "allow":
		d.Behavior = "allow"
	case decision == "deny":
		d.Behavior = "deny"
	case decision == "sAll":
		d.Behavior = "allow"
		var sugArr []json.RawMessage
		json.Unmarshal(suggestions, &sugArr)
		d.UpdatedPermissions, _ = json.Marshal(sugArr)
	case len(decision) > 1 && decision[0] == 's':
		idx := 0
		for _, ch := range decision[1:] {
			if ch < '0' || ch > '9' {
				return d, fmt.Errorf("invalid suggestion index")
			}
			idx = idx*10 + int(ch-'0')
		}
		d.Behavior = "allow"
		var sugArr []json.RawMessage
		json.Unmarshal(suggestions, &sugArr)
		if idx < len(sugArr) {
			d.UpdatedPermissions, _ = json.Marshal([]json.RawMessage{sugArr[idx]})
		}
	default:
		return d, fmt.Errorf("unknown decision: %s", decision)
	}
	if !pendingPerms.Resolve(msgID, d) {
		return d, fmt.Errorf("no pending permission for msg_id %d", msgID)
	}
	return d, nil
}

// CleanupPendingStateFunc is a callback type for cleaning up pending state.
type CleanupPendingStateFunc func(msgID int, uuid string, reason string)

// HandleStalePending checks if a pending entry is stale (hook dead or file missing).
// Returns true if stale (cleanup done), false if still alive.
func HandleStalePending(msgID int, uuid string, cleanup CleanupPendingStateFunc) bool {
	path := filepath.Join(PendingDir(), uuid+".json")
	pf, err := ReadPendingFile(path)
	if err != nil {
		cleanup(msgID, uuid, "file missing")
		return true
	}
	if pf.Status == "sent" && !IsHookAlive(pf.HookPID) {
		os.Remove(path)
		cleanup(msgID, uuid, fmt.Sprintf("hook dead (pid=%d)", pf.HookPID))
		return true
	}
	return false
}

// CleanPendingFilesBySession removes all pending files for a session.
func CleanPendingFilesBySession(sessionID string) {
	dir := PendingDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !hasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		pf, err := ReadPendingFile(path)
		if err != nil {
			continue
		}
		if pf.SessionID == sessionID {
			os.Remove(path)
			logger.Info(fmt.Sprintf("Cleaned pending file for session %s: %s", sessionID, e.Name()))
		}
	}
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}
