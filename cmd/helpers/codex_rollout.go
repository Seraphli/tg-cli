package helpers

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
)

// codexTailBytes is how much of the end of a codex rollout file is read to find the last turn event.
const codexTailBytes = 64 << 10

// CodexTurnError returns the error message of the last turn of a codex rollout file: the error.message of a
// final task_complete event. It returns "" when the last turn ended normally or a new turn has started (its
// task_started is the last turn event). Every other event is ignored: real rollouts hold token_count lines
// before a task_complete and thread_settings_applied lines after it. The error return is for a file that
// cannot be read or holds no turn event in its last 64 KiB.
func CodexTurnError(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	off := max(st.Size()-codexTailBytes, 0)
	tail := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(tail, off); err != nil && err != io.EOF {
		return "", err
	}
	lines := strings.Split(strings.TrimRight(string(tail), "\n"), "\n")
	if off > 0 {
		lines = lines[1:] // the first line starts before the tail
	}
	for i := len(lines) - 1; i >= 0; i-- {
		var ev struct {
			Payload struct {
				Type  string `json:"type"`
				Error *struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"payload"`
		}
		if json.Unmarshal([]byte(lines[i]), &ev) != nil {
			continue
		}
		switch ev.Payload.Type {
		case "task_started":
			return "", nil
		case "task_complete":
			if ev.Payload.Error == nil {
				return "", nil
			}
			return ev.Payload.Error.Message, nil
		}
	}
	return "", errors.New("no turn event in the last 64 KiB of the rollout")
}
