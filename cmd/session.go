package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/spf13/cobra"
)

var SessionCmd = &cobra.Command{
	Use:   "session",
	Short: "Manage CC sessions",
}

var (
	sessionPort        int
	sessionName        string
	sessionSelf        bool
	sessionLines       int
	sessionNoTools     bool
	sessionFormat      string
	sessionText        string
	sessionSendFrom    string
	sessionSession     string
	sessionWorkDir     string
	sessionCommand     string
	sessionHost        string
	sessionToken       string
	sessionNewName     string
	sessionPermMode    string
	sessionPermStatus  bool
	sessionNoHeader    bool
	sessionSetName     string
)

var sessionListCmd = &cobra.Command{
	Use:   "list",
	Short: "List active CC sessions",
	Run:   runSessionList,
}

var sessionLogCmd = &cobra.Command{
	Use:   "log",
	Short: "View conversation log for a session",
	Run:   runSessionLog,
}

var sessionSendCmd = &cobra.Command{
	Use:   "send",
	Short: "Send text to a session",
	Run:   runSessionSend,
}

var sessionNewCmd = &cobra.Command{
	Use:   "new",
	Short: "Launch a new CC session",
	Run:   runSessionNew,
}

var sessionExitCmd = &cobra.Command{
	Use:   "exit",
	Short: "Exit a CC session",
	Run:   runSessionExit,
}

var sessionWatchCmd = &cobra.Command{
	Use:   "watch",
	Short: "Watch a session for events (blocking)",
	Run:   runSessionWatch,
}

var sessionPermCmd = &cobra.Command{
	Use:   "perm",
	Short: "Check or switch session permission mode",
	Run:   runSessionPerm,
}

var sessionNameCmd = &cobra.Command{
	Use:   "name",
	Short: "Set or show agent name for a session",
	Run:   runSessionName,
}

var sessionCaptureCmd = &cobra.Command{
	Use:   "capture",
	Short: "Capture tmux pane content for a session",
	Run:   runSessionCapture,
}

func init() {
	SessionCmd.PersistentFlags().StringVar(&sessionHost, "host", "", "Bot API host URL (e.g., https://tg-cli.example.com)")
	SessionCmd.PersistentFlags().StringVar(&sessionToken, "token", "", "API authentication token")
	sessionListCmd.Flags().IntVar(&sessionPort, "port", 0, "Bot HTTP port (default: from config or 12500)")
	sessionLogCmd.Flags().StringVar(&sessionName, "name", "", "Agent name")
	sessionLogCmd.Flags().BoolVar(&sessionSelf, "self", false, "Auto-detect current session (uses TMUX_PANE)")
	sessionLogCmd.Flags().IntVar(&sessionLines, "lines", 20, "Number of log entries to show")
	sessionLogCmd.Flags().BoolVar(&sessionNoTools, "no-tools", false, "Exclude tool use entries")
	sessionLogCmd.Flags().StringVar(&sessionFormat, "format", "text", "Output format: text or json")
	sessionLogCmd.Flags().IntVar(&sessionPort, "port", 0, "Bot HTTP port (default: from config or 12500)")
	sessionSendCmd.Flags().StringVar(&sessionName, "name", "", "Agent name")
	sessionSendCmd.Flags().BoolVar(&sessionSelf, "self", false, "Auto-detect current session (uses TMUX_PANE)")
	sessionSendCmd.Flags().StringVar(&sessionText, "text", "", "Text to send (required)")
	sessionSendCmd.Flags().StringVar(&sessionSendFrom, "from", "", "Sender agent name (for signature)")
	sessionSendCmd.Flags().IntVar(&sessionPort, "port", 0, "Bot HTTP port (default: from config or 12500)")
	sessionSendCmd.Flags().BoolVar(&sessionNoHeader, "no-header", false, "Skip header prefix (for slash commands)")
	sessionNewCmd.Flags().StringVar(&sessionSession, "session", "", "Tmux session name")
	sessionNewCmd.Flags().StringVar(&sessionWorkDir, "workdir", "", "Working directory")
	sessionNewCmd.Flags().StringVar(&sessionCommand, "command", "", "Command to run")
	sessionNewCmd.Flags().StringVar(&sessionNewName, "name", "", "Agent name to assign after launch")
	sessionNewCmd.Flags().IntVar(&sessionPort, "port", 0, "Bot HTTP port (default: from config or 12500)")
	sessionExitCmd.Flags().StringVar(&sessionName, "name", "", "Agent name")
	sessionExitCmd.Flags().BoolVar(&sessionSelf, "self", false, "Auto-detect current session (uses TMUX_PANE)")
	sessionExitCmd.Flags().IntVar(&sessionPort, "port", 0, "Bot HTTP port (default: from config or 12500)")
	sessionWatchCmd.Flags().StringVar(&sessionName, "name", "", "Agent name to watch")
	sessionWatchCmd.Flags().IntVar(&sessionPort, "port", 0, "Bot HTTP port")
	sessionPermCmd.Flags().StringVar(&sessionName, "name", "", "Agent name")
	sessionPermCmd.Flags().BoolVar(&sessionSelf, "self", false, "Auto-detect current session (uses TMUX_PANE)")
	sessionPermCmd.Flags().StringVar(&sessionPermMode, "mode", "", "Target mode: bypass/auto/plan/default")
	sessionPermCmd.Flags().BoolVar(&sessionPermStatus, "status", false, "Show current permission mode")
	sessionPermCmd.Flags().IntVar(&sessionPort, "port", 0, "Bot HTTP port (default: from config or 12500)")
	sessionNameCmd.Flags().StringVar(&sessionName, "name", "", "Agent name to query (resolve session by current name)")
	sessionNameCmd.Flags().BoolVar(&sessionSelf, "self", false, "Auto-detect current session (uses TMUX_PANE)")
	sessionNameCmd.Flags().StringVar(&sessionSetName, "set", "", "New agent name to assign")
	sessionNameCmd.Flags().IntVar(&sessionPort, "port", 0, "Bot HTTP port (default: from config or 12500)")
	sessionCaptureCmd.Flags().StringVar(&sessionName, "name", "", "Agent name")
	sessionCaptureCmd.Flags().BoolVar(&sessionSelf, "self", false, "Auto-detect current session (uses TMUX_PANE)")
	sessionCaptureCmd.Flags().IntVar(&sessionPort, "port", 0, "Bot HTTP port (default: from config or 12500)")
	SessionCmd.AddCommand(sessionListCmd)
	SessionCmd.AddCommand(sessionLogCmd)
	SessionCmd.AddCommand(sessionSendCmd)
	SessionCmd.AddCommand(sessionNewCmd)
	SessionCmd.AddCommand(sessionExitCmd)
	SessionCmd.AddCommand(sessionWatchCmd)
	SessionCmd.AddCommand(sessionPermCmd)
	SessionCmd.AddCommand(sessionNameCmd)
	SessionCmd.AddCommand(sessionCaptureCmd)
}

// buildAPIURL constructs the full API URL from host or port
func buildAPIURL(host string, port int, path string) string {
	if host != "" {
		return strings.TrimRight(host, "/") + path
	}
	return fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
}

// apiRequest makes an HTTP request with optional Bearer token auth
func apiRequest(method, url string, body io.Reader, token string) (*http.Response, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return http.DefaultClient.Do(req)
}

func getSessionPort() int {
	if sessionPort != 0 {
		return sessionPort
	}
	creds, err := config.LoadCredentials()
	if err == nil && creds.Port != 0 {
		return creds.Port
	}
	return 12500
}

// tryResolveSessionSelf is like resolveSessionSelf but returns "" on failure instead of exiting.
func tryResolveSessionSelf(port int) string {
	pane := os.Getenv("TMUX_PANE")
	if pane == "" {
		return ""
	}
	url := buildAPIURL(sessionHost, port, "/session/list")
	resp, err := apiRequest("GET", url, nil, sessionToken)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result struct {
		Sessions []struct {
			Name   string `json:"name"`
			Target string `json:"target"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return ""
	}
	normalizedPane := strings.TrimPrefix(pane, "%")
	for _, s := range result.Sessions {
		target := strings.TrimPrefix(s.Target, "%")
		if target == normalizedPane || s.Target == pane {
			return s.Name
		}
	}
	return ""
}

// resolveSessionSelf queries /session/list and finds the agent name for the current TMUX_PANE.
func resolveSessionSelf(port int) string {
	pane := os.Getenv("TMUX_PANE")
	if pane == "" {
		fmt.Fprintln(os.Stderr, "Error: --self requires TMUX_PANE environment variable")
		os.Exit(1)
	}
	url := buildAPIURL(sessionHost, port, "/session/list")
	resp, err := apiRequest("GET", url, nil, sessionToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot connect to bot: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result struct {
		Sessions []struct {
			Name   string `json:"name"`
			Target string `json:"target"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to parse session list: %v\n", err)
		os.Exit(1)
	}
	// Normalize pane ID for comparison (strip leading % if present)
	normalizedPane := strings.TrimPrefix(pane, "%")
	for _, s := range result.Sessions {
		target := strings.TrimPrefix(s.Target, "%")
		if target == normalizedPane || s.Target == pane {
			return s.Name
		}
	}
	fmt.Fprintf(os.Stderr, "Error: no session found for pane %s\n", pane)
	os.Exit(1)
	return ""
}

func runSessionList(cmd *cobra.Command, args []string) {
	port := getSessionPort()
	url := buildAPIURL(sessionHost, port, "/session/list")
	resp, err := apiRequest("GET", url, nil, sessionToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot connect to bot: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result struct {
		Sessions []struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			Target     string `json:"target"`
			CWD        string `json:"cwd"`
			ProjectDir string `json:"project_dir"`
			Running    bool   `json:"running"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to parse response: %v\n", err)
		os.Exit(1)
	}
	if len(result.Sessions) == 0 {
		fmt.Println("No active sessions.")
		return
	}
	for _, s := range result.Sessions {
		runningTag := ""
		if s.Running {
			runningTag = " [running]"
		}
		nameTag := ""
		if s.Name != "" {
			nameTag = fmt.Sprintf(" name=%s", s.Name)
		}
		sid := s.ID
		if len(sid) > 8 {
			sid = sid[:8]
		}
		fmt.Printf("[%s]%s target=%s cwd=%s%s\n", sid, nameTag, s.Target, s.CWD, runningTag)
	}
}

func runSessionLog(cmd *cobra.Command, args []string) {
	port := getSessionPort()
	name := sessionName
	if sessionSelf {
		name = resolveSessionSelf(port)
	}
	if name == "" {
		fmt.Fprintln(os.Stderr, "Error: --name or --self is required")
		os.Exit(1)
	}
	path := fmt.Sprintf("/session/log?name=%s&lines=%d", name, sessionLines)
	if sessionNoTools {
		path += "&no_tools=true"
	}
	url := buildAPIURL(sessionHost, port, path)
	resp, err := apiRequest("GET", url, nil, sessionToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot connect to bot: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		fmt.Fprintf(os.Stderr, "Error: %s\n", string(body))
		os.Exit(1)
	}
	if sessionFormat == "json" {
		fmt.Println(string(body))
		return
	}
	var response struct {
		Target       string `json:"target"`
		ContextPct   int    `json:"context_pct"`
		ContextUsed  string `json:"context_used"`
		ContextTotal string `json:"context_total"`
		Messages     []struct {
			Type       string `json:"type"`
			Timestamp  string `json:"timestamp"`
			Text       string `json:"text"`
			Tool       string `json:"tool"`
			ToolDetail string `json:"tool_detail"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to parse response: %v\n", err)
		os.Exit(1)
	}
	sep := "────────────────────────"
	ctxStr := ""
	if response.ContextUsed != "" {
		ctxStr = fmt.Sprintf(" | 📊 Context: %d%% (%s/%s)", response.ContextPct, response.ContextUsed, response.ContextTotal)
	}
	fmt.Printf("📟 %s%s\n", response.Target, ctxStr)
	for _, m := range response.Messages {
		fmt.Println(sep)
		// Format timestamp
		tsStr := m.Timestamp
		if t, err := time.Parse(time.RFC3339, m.Timestamp); err == nil {
			tsStr = t.Format("2006-01-02 15:04:05")
		}
		header := fmt.Sprintf("%s [%s]", tsStr, m.Type)
		if m.Tool != "" {
			header += fmt.Sprintf(" [%s]", m.Tool)
		}
		fmt.Println(header)
		if m.Text != "" {
			fmt.Println(m.Text)
		}
		if m.ToolDetail != "" && m.ToolDetail != m.Tool {
			fmt.Println(m.ToolDetail)
		}
	}
	if len(response.Messages) > 0 {
		fmt.Println(sep)
	}
}

func runSessionSend(cmd *cobra.Command, args []string) {
	port := getSessionPort()
	name := sessionName
	if sessionSelf {
		name = resolveSessionSelf(port)
	}
	if name == "" {
		fmt.Fprintln(os.Stderr, "Error: --name or --self is required")
		os.Exit(1)
	}
	if sessionText == "" {
		fmt.Fprintln(os.Stderr, "Error: --text is required")
		os.Exit(1)
	}
	from := sessionSendFrom
	if from == "" && sessionSelf {
		from = resolveSessionSelf(port)
	}
	// Auto-detect sender from TMUX_PANE if from is still empty (best-effort, don't exit on failure)
	if from == "" && os.Getenv("TMUX_PANE") != "" {
		from = tryResolveSessionSelf(port)
	}
	body := map[string]interface{}{
		"name": name,
		"text": sessionText,
	}
	if from != "" {
		body["from"] = from
	}
	if sessionNoHeader {
		body["noHeader"] = true
	}
	data, _ := json.Marshal(body)
	url := buildAPIURL(sessionHost, port, "/session/send")
	resp, err := apiRequest("POST", url, bytes.NewReader(data), sessionToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot connect to bot: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "Error: %s\n", string(respBody))
		os.Exit(1)
	}
	fmt.Printf("Message sent to %s.\n", name)
}

func runSessionNew(cmd *cobra.Command, args []string) {
	port := getSessionPort()
	if sessionSession == "" || sessionWorkDir == "" {
		fmt.Fprintln(os.Stderr, "Error: --session and --workdir required")
		os.Exit(1)
	}
	reqBody := struct {
		Session string `json:"session,omitempty"`
		WorkDir string `json:"workdir,omitempty"`
		Command string `json:"command,omitempty"`
		Name    string `json:"name,omitempty"`
	}{sessionSession, sessionWorkDir, sessionCommand, sessionNewName}
	data, _ := json.Marshal(reqBody)
	url := buildAPIURL(sessionHost, port, "/session/new")
	resp, err := apiRequest("POST", url, bytes.NewReader(data), sessionToken)
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
	fmt.Println("Session launched.")
}

func runSessionWatch(cmd *cobra.Command, args []string) {
	port := getSessionPort()
	if sessionName == "" {
		fmt.Fprintln(os.Stderr, "Error: --name required")
		os.Exit(1)
	}
	url := buildAPIURL(sessionHost, port, fmt.Sprintf("/session/watch?name=%s", sessionName))
	resp, err := apiRequest("GET", url, nil, sessionToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	fmt.Println(string(body))
}

func runSessionPerm(cmd *cobra.Command, args []string) {
	port := getSessionPort()
	name := sessionName
	if sessionSelf {
		name = resolveSessionSelf(port)
	}
	if name == "" {
		fmt.Fprintln(os.Stderr, "Error: --name or --self is required")
		os.Exit(1)
	}
	if !sessionPermStatus && sessionPermMode == "" {
		fmt.Fprintln(os.Stderr, "Error: --mode or --status is required")
		os.Exit(1)
	}
	// Resolve name to tmux target via /session/list
	listURL := buildAPIURL(sessionHost, port, "/session/list")
	listResp, err := apiRequest("GET", listURL, nil, sessionToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot connect to bot: %v\n", err)
		os.Exit(1)
	}
	defer listResp.Body.Close()
	var listResult struct {
		Sessions []struct {
			Name   string `json:"name"`
			Target string `json:"target"`
		} `json:"sessions"`
	}
	json.NewDecoder(listResp.Body).Decode(&listResult)
	var target string
	for _, s := range listResult.Sessions {
		if s.Name == name {
			target = s.Target
			break
		}
	}
	if target == "" {
		fmt.Fprintf(os.Stderr, "Error: session %q not found\n", name)
		os.Exit(1)
	}
	if sessionPermStatus {
		statusURL := buildAPIURL(sessionHost, port, "/perm/status") + "?target=" + url.QueryEscape(target)
		resp, err := apiRequest("GET", statusURL, nil, sessionToken)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		defer resp.Body.Close()
		var result map[string]string
		json.NewDecoder(resp.Body).Decode(&result)
		if result["status"] == "error" {
			fmt.Fprintf(os.Stderr, "Error: %s\n", result["message"])
			os.Exit(1)
		}
		fmt.Printf("Permission mode: %s\n", result["mode"])
	} else {
		switchURL := buildAPIURL(sessionHost, port, "/perm/switch") + "?target=" + url.QueryEscape(target) + "&mode=" + url.QueryEscape(sessionPermMode)
		resp, err := apiRequest("GET", switchURL, nil, sessionToken)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		defer resp.Body.Close()
		var result map[string]string
		json.NewDecoder(resp.Body).Decode(&result)
		if result["status"] == "error" {
			fmt.Fprintf(os.Stderr, "Error: %s\n", result["message"])
			os.Exit(1)
		}
		fmt.Printf("Switched to: %s\n", result["mode"])
	}
}

func runSessionExit(cmd *cobra.Command, args []string) {
	port := getSessionPort()
	name := sessionName
	if sessionSelf {
		name = resolveSessionSelf(port)
	}
	if name == "" {
		fmt.Fprintln(os.Stderr, "Error: --name or --self is required")
		os.Exit(1)
	}
	reqBody := struct {
		Name string `json:"name"`
	}{name}
	data, _ := json.Marshal(reqBody)
	url := buildAPIURL(sessionHost, port, "/session/exit")
	resp, err := apiRequest("POST", url, bytes.NewReader(data), sessionToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot connect to bot: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "Error: %s\n", string(respBody))
		os.Exit(1)
	}
	fmt.Println("Session exit initiated.")
}

func runSessionCapture(cmd *cobra.Command, args []string) {
	port := getSessionPort()
	name := sessionName
	if sessionSelf {
		name = resolveSessionSelf(port)
	}
	if name == "" {
		fmt.Fprintln(os.Stderr, "Error: --name or --self is required")
		os.Exit(1)
	}
	listURL := buildAPIURL(sessionHost, port, "/session/list")
	listResp, err := apiRequest("GET", listURL, nil, sessionToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot connect to bot: %v\n", err)
		os.Exit(1)
	}
	defer listResp.Body.Close()
	var listResult struct {
		Sessions []struct {
			Name   string `json:"name"`
			Target string `json:"target"`
		} `json:"sessions"`
	}
	json.NewDecoder(listResp.Body).Decode(&listResult)
	var target string
	for _, s := range listResult.Sessions {
		if s.Name == name {
			target = s.Target
			break
		}
	}
	if target == "" {
		fmt.Fprintf(os.Stderr, "Error: session %q not found\n", name)
		os.Exit(1)
	}
	captureURL := buildAPIURL(sessionHost, port, "/capture") + "?target=" + url.QueryEscape(target)
	resp, err := apiRequest("GET", captureURL, nil, sessionToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "Error: %s\n", string(body))
		os.Exit(1)
	}
	var result struct {
		Content string `json:"content"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	fmt.Print(result.Content)
}

func runSessionName(cmd *cobra.Command, args []string) {
	port := getSessionPort()
	listURL := buildAPIURL(sessionHost, port, "/session/list")
	listResp, err := apiRequest("GET", listURL, nil, sessionToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot connect to bot: %v\n", err)
		os.Exit(1)
	}
	defer listResp.Body.Close()
	listBody, _ := io.ReadAll(listResp.Body)
	var listResult struct {
		Sessions []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Target string `json:"target"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(listBody, &listResult); err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to parse session list: %v\n", err)
		os.Exit(1)
	}
	var sessionID, currentName string
	if sessionSelf {
		pane := os.Getenv("TMUX_PANE")
		if pane == "" {
			fmt.Fprintln(os.Stderr, "Error: --self requires TMUX_PANE environment variable")
			os.Exit(1)
		}
		normalizedPane := strings.TrimPrefix(pane, "%")
		for _, s := range listResult.Sessions {
			target := strings.TrimPrefix(s.Target, "%")
			if target == normalizedPane || s.Target == pane {
				sessionID = s.ID
				currentName = s.Name
				break
			}
		}
		if sessionID == "" {
			fmt.Fprintf(os.Stderr, "Error: no session found for pane %s\n", pane)
			os.Exit(1)
		}
	} else if sessionName != "" {
		for _, s := range listResult.Sessions {
			if s.Name == sessionName {
				sessionID = s.ID
				currentName = s.Name
				break
			}
		}
		if sessionID == "" {
			fmt.Fprintf(os.Stderr, "Error: session %q not found\n", sessionName)
			os.Exit(1)
		}
	} else {
		fmt.Fprintln(os.Stderr, "Error: --name or --self is required")
		os.Exit(1)
	}
	if sessionSetName == "" {
		if currentName != "" {
			fmt.Printf("Session %s name: %s\n", sessionID[:8], currentName)
		} else {
			fmt.Printf("Session %s has no name\n", sessionID[:8])
		}
		return
	}
	nameURL := buildAPIURL(sessionHost, port, fmt.Sprintf("/session/name?session_id=%s&name=%s",
		url.QueryEscape(sessionID), url.QueryEscape(sessionSetName)))
	resp, err := apiRequest("GET", nameURL, nil, sessionToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "Error: %s\n", string(body))
		os.Exit(1)
	}
	fmt.Printf("Session %s named '%s'\n", sessionID[:8], sessionSetName)
}
