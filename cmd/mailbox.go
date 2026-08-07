package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/spf13/cobra"
)

var MailboxCmd = &cobra.Command{
	Use:   "mailbox",
	Short: "Agent mailbox for inter-agent communication",
}

var (
	mailboxTo         string
	mailboxText       string
	mailboxFrom       string
	mailboxSelf       bool
	mailboxName       string
	mailboxPortFlag   int
	mailboxHost       string
	mailboxToken      string
	mailboxSubject    string
	mailboxFile       string
	mailboxDownloadID string
	mailboxJSON       bool
)

var mailboxSendCmd = &cobra.Command{
	Use:   "send",
	Short: "Send a message to another agent",
	Run:   runMailboxSend,
}

var mailboxReceiveCmd = &cobra.Command{
	Use:   "receive",
	Short: "Wait (long-poll) for the next incoming message",
	Run:   runMailboxReceive,
}

var mailboxInboxCmd = &cobra.Command{
	Use:   "inbox",
	Short: "List all messages in the mailbox",
	Run:   runMailboxInbox,
}

var mailboxDownloadCmd = &cobra.Command{
	Use:   "download",
	Short: "Download attachment from a mailbox message",
	Run:   runMailboxDownload,
}

func init() {
	MailboxCmd.PersistentFlags().StringVar(&mailboxHost, "host", "", "Bot API host URL")
	MailboxCmd.PersistentFlags().StringVar(&mailboxToken, "token", "", "API authentication token")

	mailboxSendCmd.Flags().StringVar(&mailboxTo, "to", "", "Target agent name (required)")
	mailboxSendCmd.Flags().StringVar(&mailboxText, "text", "", "Message content (required)")
	mailboxSendCmd.Flags().StringVar(&mailboxFrom, "from", "", "Sender name")
	mailboxSendCmd.Flags().BoolVar(&mailboxSelf, "self", false, "Auto-detect sender name from TMUX_PANE")
	mailboxSendCmd.Flags().IntVar(&mailboxPortFlag, "port", 0, "Bot HTTP port (default: from config or 12500)")
	mailboxSendCmd.Flags().StringVar(&mailboxSubject, "subject", "", "Email subject (required)")
	mailboxSendCmd.Flags().StringVar(&mailboxFile, "file", "", "Attachment file path")

	mailboxReceiveCmd.Flags().BoolVar(&mailboxSelf, "self", false, "Auto-detect agent name from TMUX_PANE")
	mailboxReceiveCmd.Flags().StringVar(&mailboxName, "name", "", "Agent name to receive for")
	mailboxReceiveCmd.Flags().IntVar(&mailboxPortFlag, "port", 0, "Bot HTTP port (default: from config or 12500)")

	mailboxInboxCmd.Flags().BoolVar(&mailboxSelf, "self", false, "Auto-detect agent name from TMUX_PANE")
	mailboxInboxCmd.Flags().StringVar(&mailboxName, "name", "", "Agent name to list inbox for")
	mailboxInboxCmd.Flags().IntVar(&mailboxPortFlag, "port", 0, "Bot HTTP port (default: from config or 12500)")
	mailboxInboxCmd.Flags().BoolVar(&mailboxJSON, "json", false, "Output raw JSON from HTTP API instead of human-readable format")

	mailboxDownloadCmd.Flags().StringVar(&mailboxDownloadID, "id", "", "Message ID")
	mailboxDownloadCmd.Flags().IntVar(&mailboxPortFlag, "port", 0, "Bot HTTP port")

	MailboxCmd.AddCommand(mailboxSendCmd)
	MailboxCmd.AddCommand(mailboxReceiveCmd)
	MailboxCmd.AddCommand(mailboxInboxCmd)
	MailboxCmd.AddCommand(mailboxDownloadCmd)
}

func getMailboxPort() int {
	if mailboxPortFlag != 0 {
		return mailboxPortFlag
	}
	creds, err := config.LoadCredentials()
	if err == nil && creds.Port != 0 {
		return creds.Port
	}
	return 12500
}

func runMailboxSend(cmd *cobra.Command, args []string) {
	if mailboxTo == "" || mailboxText == "" {
		fmt.Fprintln(os.Stderr, "Error: --to and --text are required")
		os.Exit(1)
	}
	if mailboxSubject == "" {
		fmt.Fprintln(os.Stderr, "Error: --subject is required")
		os.Exit(1)
	}
	port := getMailboxPort()
	from := mailboxFrom
	if mailboxSelf {
		from = resolveSessionSelf(port)
	}
	if from == "" {
		fmt.Fprintln(os.Stderr, "Error: --from or --self is required")
		os.Exit(1)
	}
	// Build multipart request
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	writer.WriteField("from", from)
	writer.WriteField("to", mailboxTo)
	writer.WriteField("subject", mailboxSubject)
	writer.WriteField("text", mailboxText)
	if mailboxFile != "" {
		f, err := os.Open(mailboxFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: cannot open file: %v\n", err)
			os.Exit(1)
		}
		defer f.Close()
		part, err := writer.CreateFormFile("file", filepath.Base(mailboxFile))
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		io.Copy(part, f)
	}
	writer.Close()
	url := buildAPIURL(mailboxHost, port, "/mailbox/send")
	req, _ := http.NewRequest("POST", url, &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	if mailboxToken != "" {
		req.Header.Set("Authorization", "Bearer "+mailboxToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot connect to bot: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		fmt.Fprintf(os.Stderr, "Error: %s\n", string(respBody))
		os.Exit(1)
	}
	var result struct {
		OK bool   `json:"ok"`
		ID string `json:"id"`
	}
	json.Unmarshal(respBody, &result)
	fmt.Printf("Message sent to %s (id: %s)\n", mailboxTo, result.ID)
}

func runMailboxReceive(cmd *cobra.Command, args []string) {
	port := getMailboxPort()
	name := mailboxName
	if mailboxSelf {
		name = resolveSessionSelf(port)
	}
	if name == "" {
		fmt.Fprintln(os.Stderr, "Error: --self or --name is required")
		os.Exit(1)
	}
	// Set up context with cancellation on Ctrl+C
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()
	// Long-poll with upgrade-aware reconnect: a drop during a service upgrade auto-reconnects; a drop
	// with no upgrade in progress is abnormal and exits with an error so the agent can investigate.
	apiURL := buildAPIURL(mailboxHost, port, fmt.Sprintf("/mailbox/receive?name=%s", name))
	resp, err := getWithUpgradeReconnect(ctx, apiURL, mailboxToken)
	if err != nil {
		if ctx.Err() != nil {
			return // cancelled by user
		}
		fmt.Fprintln(os.Stderr, "Connection lost: server unreachable (not an upgrade)")
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result struct {
		OK       bool   `json:"ok"`
		Error    string `json:"error"`
		Messages []struct {
			ID        string    `json:"id"`
			From      string    `json:"from"`
			Subject   string    `json:"subject"`
			Text      string    `json:"text"`
			FileName  string    `json:"file_name"`
			Timestamp time.Time `json:"timestamp"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot parse response: %v\n", err)
		os.Exit(1)
	}
	if !result.OK && result.Error != "" {
		fmt.Fprintf(os.Stderr, "Error: %s\n", result.Error)
		os.Exit(1)
	}
	if len(result.Messages) == 0 {
		fmt.Println("No new messages.")
		return
	}
	for _, m := range result.Messages {
		fmt.Printf("────────────────────────\n")
		fmt.Printf("[%s] %s\n", m.From, m.Timestamp.Format("2006-01-02 15:04:05"))
		if m.Subject != "" {
			fmt.Printf("Subject: %s\n", m.Subject)
		}
		fmt.Printf("%s\n", m.Text)
		if m.FileName != "" {
			fmt.Printf("📎 %s (download: tg-cli mailbox download --id %s)\n", m.FileName, m.ID)
		}
	}
}

func runMailboxInbox(cmd *cobra.Command, args []string) {
	port := getMailboxPort()
	name := mailboxName
	if mailboxSelf {
		name = resolveSessionSelf(port)
	}
	if name == "" {
		fmt.Fprintln(os.Stderr, "Error: --self or --name is required")
		os.Exit(1)
	}
	url := buildAPIURL(mailboxHost, port, fmt.Sprintf("/mailbox/inbox?name=%s", name))
	resp, err := apiRequest("GET", url, nil, mailboxToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot connect to bot: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if mailboxJSON {
		os.Stdout.Write(body)
		return
	}
	var result struct {
		Messages []struct {
			ID        string    `json:"id"`
			From      string    `json:"from"`
			Subject   string    `json:"subject"`
			Text      string    `json:"text"`
			FileName  string    `json:"file_name"`
			Timestamp time.Time `json:"timestamp"`
			Read      bool      `json:"read"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot parse response: %v\n", err)
		os.Exit(1)
	}
	if len(result.Messages) == 0 {
		fmt.Println("No messages.")
		return
	}
	for _, m := range result.Messages {
		prefix := " "
		if !m.Read {
			prefix = "*"
		}
		attach := ""
		if m.FileName != "" {
			attach = " 📎" + m.FileName
		}
		fmt.Printf("%s %s [%s] %s %s%s\n", prefix, m.ID, m.From, m.Timestamp.Format(time.RFC3339), m.Text, attach)
	}
}

func runMailboxDownload(cmd *cobra.Command, args []string) {
	if mailboxDownloadID == "" {
		fmt.Fprintln(os.Stderr, "Error: --id is required")
		os.Exit(1)
	}
	port := getMailboxPort()
	url := buildAPIURL(mailboxHost, port, fmt.Sprintf("/mailbox/download?id=%s", mailboxDownloadID))
	resp, err := apiRequest("GET", url, nil, mailboxToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot connect to bot: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "Error: %s\n", string(body))
		os.Exit(1)
	}
	// Parse filename from Content-Disposition header
	cd := resp.Header.Get("Content-Disposition")
	fileName := "attachment"
	if cd != "" {
		parts := strings.Split(cd, "filename=")
		if len(parts) > 1 {
			fileName = strings.Trim(parts[1], "\"")
		}
	}
	// Save to current directory
	f, err := os.Create(fileName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot create file: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()
	n, _ := io.Copy(f, resp.Body)
	fmt.Printf("Downloaded: %s (%d bytes)\n", fileName, n)
}
