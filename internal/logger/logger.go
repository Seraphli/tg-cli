package logger

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Seraphli/tg-cli/internal/config"
)

var debugMode bool

func SetDebugMode(enabled bool) {
	debugMode = enabled
}

func IsDebugMode() bool {
	return debugMode
}

func getLogPath() string {
	return filepath.Join(config.GetConfigDir(), "bot.log")
}

func ensureLogDir() error {
	logPath := getLogPath()
	dir := filepath.Dir(logPath)
	return os.MkdirAll(dir, 0755)
}

func formatEntry(level, message string) string {
	ts := time.Now().Format(time.RFC3339)
	pid := os.Getpid()
	return fmt.Sprintf("[%s] [PID=%d] [%s] %s", ts, pid, level, message)
}

func Info(message string) {
	entry := formatEntry("INFO", message)
	fmt.Println(entry)
	if debugMode {
		ensureLogDir()
		f, err := os.OpenFile(getLogPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err == nil {
			defer f.Close()
			f.WriteString(entry + "\n")
		}
	}
}

func Debug(message string) {
	if !debugMode {
		return
	}
	entry := formatEntry("DEBUG", message)
	fmt.Println(entry)
	ensureLogDir()
	f, err := os.OpenFile(getLogPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err == nil {
		defer f.Close()
		f.WriteString(entry + "\n")
	}
}

func Error(message string) {
	entry := formatEntry("ERROR", message)
	fmt.Fprintln(os.Stderr, entry)
	ensureLogDir()
	f, err := os.OpenFile(getLogPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err == nil {
		defer f.Close()
		f.WriteString(entry + "\n")
	}
}
