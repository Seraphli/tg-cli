package logger

import (
	"fmt"
	"io"
	"os"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
)

var (
	debugMode bool
	writer    io.Writer
)

// Init initializes the logger with a lumberjack rotating file writer.
func Init(logPath string, debug bool) {
	debugMode = debug
	writer = &lumberjack.Logger{
		Filename:   logPath,
		MaxSize:    10, // MB
		MaxBackups: 5,
		Compress:   false,
	}
}

func SetDebugMode(enabled bool) {
	debugMode = enabled
}

func IsDebugMode() bool {
	return debugMode
}

func formatEntry(level, message string) string {
	ts := time.Now().Format(time.RFC3339)
	pid := os.Getpid()
	return fmt.Sprintf("[%s] [PID=%d] [%s] %s", ts, pid, level, message)
}

func writeLog(entry string) {
	if writer == nil {
		return
	}
	writer.Write([]byte(entry + "\n"))
}

func Info(message string) {
	entry := formatEntry("INFO", message)
	fmt.Println(entry)
	if debugMode {
		writeLog(entry)
	}
}

func Debug(message string) {
	if !debugMode {
		return
	}
	entry := formatEntry("DEBUG", message)
	fmt.Println(entry)
	writeLog(entry)
}

func Error(message string) {
	entry := formatEntry("ERROR", message)
	fmt.Fprintln(os.Stderr, entry)
	writeLog(entry)
}
