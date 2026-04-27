package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/spf13/cobra"
)

var SendFileCmd = &cobra.Command{
	Use:   "send-file",
	Short: "Send a file to Telegram chat",
	RunE:  runSendFile,
}

var (
	sendFilePort int
	sendFilePath string
	sendCaption  string
)

func init() {
	SendFileCmd.Flags().IntVar(&sendFilePort, "port", 0, "HTTP server port (overrides config)")
	SendFileCmd.Flags().StringVar(&sendFilePath, "file", "", "Absolute path to the file to send")
	SendFileCmd.Flags().StringVar(&sendCaption, "caption", "", "Optional caption for the file")
	SendFileCmd.MarkFlagRequired("file")
}

func runSendFile(cmd *cobra.Command, args []string) error {
	creds, err := config.LoadCredentials()
	if err != nil {
		return fmt.Errorf("load credentials: %w", err)
	}
	port := sendFilePort
	if port == 0 {
		port = creds.Port
	}
	if port == 0 {
		port = 12500
	}
	info, err := os.Stat(sendFilePath)
	if err != nil {
		return fmt.Errorf("file not found: %s", sendFilePath)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular file: %s", sendFilePath)
	}
	tmuxTarget := ""
	if pane := os.Getenv("TMUX_PANE"); pane != "" {
		tmuxTarget = pane
		if tmuxEnv := os.Getenv("TMUX"); tmuxEnv != "" {
			parts := strings.SplitN(tmuxEnv, ",", 2)
			tmuxTarget = pane + "@" + parts[0]
		}
	}
	cwd, _ := os.Getwd()
	body, _ := json.Marshal(map[string]string{
		"file_path":   sendFilePath,
		"caption":     sendCaption,
		"tmux_target": tmuxTarget,
		"cwd":         cwd,
	})
	resp, err := http.Post(
		fmt.Sprintf("http://127.0.0.1:%d/file/send", port),
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		return fmt.Errorf("bot unreachable: %v", err)
	}
	defer resp.Body.Close()
	var result struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bot returned HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return fmt.Errorf("invalid response from bot: %s", string(respBody))
	}
	if !result.OK {
		return fmt.Errorf("send failed: %s", result.Error)
	}
	fmt.Println(result.Message)
	return nil
}
