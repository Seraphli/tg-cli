package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/spf13/cobra"
)

var CronCmd = &cobra.Command{
	Use:   "cron",
	Short: "Manage cron jobs",
}

var (
	cronMode      string
	cronSchedule  string
	cronPrompt    string
	cronAgent     string
	cronCWD       string
	cronOnce      bool
	cronPort      int
	cronJobID     string
	cronSelf      bool
	cronName      string
	cronMaxTurns  int
	cronHost      string
	cronToken     string
	cronNoHeader  bool
	cronFresh     bool
)

var cronAddCmd = &cobra.Command{
	Use:   "add",
	Short: "Add a new cron job",
	Run:   runCronAdd,
}

var cronListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all cron jobs",
	Run:   runCronList,
}

var cronRemoveCmd = &cobra.Command{
	Use:   "remove",
	Short: "Remove a cron job",
	Run:   runCronRemove,
}

var cronUpdateCmd = &cobra.Command{
	Use:   "update",
	Short: "Update an existing cron job",
	Run:   runCronUpdate,
}

var cronLogCmd = &cobra.Command{
	Use:   "log",
	Short: "View print job conversation transcript",
	Run:   runCronLog,
}

var cronPauseCmd = &cobra.Command{
	Use:   "pause",
	Short: "Pause a cron job",
	Run:   runCronPause,
}

var cronResumeCmd = &cobra.Command{
	Use:   "resume",
	Short: "Resume a paused cron job",
	Run:   runCronResume,
}

func init() {
	CronCmd.PersistentFlags().StringVar(&cronHost, "host", "", "Bot API host URL")
	CronCmd.PersistentFlags().StringVar(&cronToken, "token", "", "API authentication token")
	cronAddCmd.Flags().StringVar(&cronMode, "mode", "", "Execution mode: print or inject (required)")
	cronAddCmd.Flags().StringVar(&cronSchedule, "schedule", "", "Schedule: duration (30m, 2h) or cron expression (required)")
	cronAddCmd.Flags().StringVar(&cronPrompt, "prompt", "", "Prompt text to send/inject (required)")
	cronAddCmd.Flags().StringVar(&cronAgent, "agent", "", "Agent name (required for inject mode)")
	cronAddCmd.Flags().StringVar(&cronCWD, "cwd", "", "Working directory for print mode (default: current dir)")
	cronAddCmd.Flags().BoolVar(&cronOnce, "once", false, "Single execution, auto-delete after run")
	cronAddCmd.Flags().IntVar(&cronPort, "port", 0, "Bot HTTP port (default: from config or 12500)")
	cronAddCmd.Flags().BoolVar(&cronSelf, "self", false, "Auto-detect current session (uses TMUX_PANE)")
	cronAddCmd.Flags().StringVar(&cronName, "name", "", "Human-friendly name for this job (unique)")
	cronAddCmd.Flags().IntVar(&cronMaxTurns, "max-turns", 0, "Max agentic turns for print mode (0 = no limit)")
	cronAddCmd.Flags().BoolVar(&cronNoHeader, "no-header", false, "Skip cron header prefix in inject mode")
	cronAddCmd.Flags().BoolVar(&cronFresh, "fresh", false, "Create new session each run (print mode, no resume)")
	cronRemoveCmd.Flags().StringVar(&cronJobID, "id", "", "Job ID or name to remove")
	cronRemoveCmd.Flags().StringVar(&cronName, "name", "", "Job name to remove")
	cronRemoveCmd.Flags().IntVar(&cronPort, "port", 0, "Bot HTTP port")
	cronListCmd.Flags().IntVar(&cronPort, "port", 0, "Bot HTTP port")
	cronUpdateCmd.Flags().StringVar(&cronJobID, "id", "", "Job ID or name to update")
	cronUpdateCmd.Flags().StringVar(&cronPrompt, "prompt", "", "New prompt")
	cronUpdateCmd.Flags().StringVar(&cronSchedule, "schedule", "", "New schedule")
	cronUpdateCmd.Flags().StringVar(&cronAgent, "agent", "", "New agent name")
	cronUpdateCmd.Flags().StringVar(&cronName, "name", "", "New name")
	cronUpdateCmd.Flags().IntVar(&cronPort, "port", 0, "Bot HTTP port")
	cronLogCmd.Flags().StringVar(&cronJobID, "id", "", "Job ID or name")
	cronLogCmd.Flags().StringVar(&cronName, "name", "", "Job name")
	cronLogCmd.Flags().IntVar(&cronPort, "port", 0, "Bot HTTP port")
	cronPauseCmd.Flags().StringVar(&cronJobID, "id", "", "Job ID or name to pause")
	cronPauseCmd.Flags().StringVar(&cronName, "name", "", "Job name to pause")
	cronPauseCmd.Flags().IntVar(&cronPort, "port", 0, "Bot HTTP port")
	cronResumeCmd.Flags().StringVar(&cronJobID, "id", "", "Job ID or name to resume")
	cronResumeCmd.Flags().StringVar(&cronName, "name", "", "Job name to resume")
	cronResumeCmd.Flags().IntVar(&cronPort, "port", 0, "Bot HTTP port")
	CronCmd.AddCommand(cronAddCmd)
	CronCmd.AddCommand(cronListCmd)
	CronCmd.AddCommand(cronRemoveCmd)
	CronCmd.AddCommand(cronUpdateCmd)
	CronCmd.AddCommand(cronLogCmd)
	CronCmd.AddCommand(cronPauseCmd)
	CronCmd.AddCommand(cronResumeCmd)
}

func getCronPort() int {
	if cronPort > 0 {
		return cronPort
	}
	creds, err := config.LoadCredentials()
	if err == nil && creds.Port > 0 {
		return creds.Port
	}
	return 12500
}

func runCronAdd(cmd *cobra.Command, args []string) {
	if cronMode == "" || cronSchedule == "" || cronPrompt == "" {
		fmt.Fprintln(os.Stderr, "Error: --mode, --schedule, and --prompt are required")
		os.Exit(1)
	}
	if cronMode == "inject" && cronAgent == "" && !cronSelf {
		fmt.Fprintln(os.Stderr, "Error: --agent or --self is required for inject mode")
		os.Exit(1)
	}
	if cronCWD == "" {
		cronCWD, _ = os.Getwd()
	}
	if _, err := time.ParseDuration(cronSchedule); err != nil {
		fields := strings.Fields(cronSchedule)
		if len(fields) != 5 {
			fmt.Fprintln(os.Stderr, "Error: invalid schedule format. Use duration (30m, 2h) or 5-field cron expression")
			os.Exit(1)
		}
	}
	// Resolve tmux target from current session if --self flag is set
	tmuxTarget := ""
	if cronSelf {
		pane := os.Getenv("TMUX_PANE")
		tmux := os.Getenv("TMUX")
		if pane == "" {
			fmt.Fprintln(os.Stderr, "Error: TMUX_PANE not set, are you inside a tmux session?")
			os.Exit(1)
		}
		_ = tmux
		tmuxTarget = pane
	}
	port := getCronPort()
	reqBody := struct {
		Mode       string `json:"mode"`
		Schedule   string `json:"schedule"`
		Once       bool   `json:"once"`
		Prompt     string `json:"prompt"`
		AgentName  string `json:"agent_name"`
		TmuxTarget string `json:"tmux_target,omitempty"`
		Name       string `json:"name,omitempty"`
		CWD        string `json:"cwd"`
		MaxTurns   int    `json:"max_turns,omitempty"`
		NoHeader   bool   `json:"no_header,omitempty"`
		Fresh      bool   `json:"fresh,omitempty"`
	}{cronMode, cronSchedule, cronOnce, cronPrompt, cronAgent, tmuxTarget, cronName, cronCWD, cronMaxTurns, cronNoHeader, cronFresh}
	data, _ := json.Marshal(reqBody)
	url := buildAPIURL(cronHost, port, "/cron/add")
	resp, err := apiRequest("POST", url, bytes.NewReader(data), cronToken)
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
	fmt.Printf("Cron job created: %s\n", result.ID)
}

func runCronList(cmd *cobra.Command, args []string) {
	port := getCronPort()
	url := buildAPIURL(cronHost, port, "/cron/list")
	resp, err := apiRequest("GET", url, nil, cronToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot connect to bot: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result struct {
		Jobs []cronJob `json:"jobs"`
	}
	json.Unmarshal(body, &result)
	if len(result.Jobs) == 0 {
		fmt.Println("No cron jobs configured.")
		return
	}
	for _, j := range result.Jobs {
		onceTag := ""
		if j.Once {
			onceTag = " [once]"
		}
		pausedTag := ""
		if j.Paused {
			pausedTag = " [paused]"
		}
		freshTag := ""
		if j.Fresh {
			freshTag = " [fresh]"
		}
		sessionInfo := ""
		if j.SessionID != "" {
			sid := j.SessionID
			if len(sid) > 8 {
				sid = sid[:8]
			}
			sessionInfo = fmt.Sprintf(" session=%s", sid)
		}
		nameInfo := ""
		if j.Name != "" {
			nameInfo = fmt.Sprintf(" name=%s", j.Name)
		}
		fmt.Printf("[%s] mode=%s schedule=%s%s%s%s%s%s prompt=%s\n", j.ID[:8], j.Mode, j.Schedule, onceTag, pausedTag, freshTag, nameInfo, sessionInfo, j.Prompt)
	}
}

func runCronRemove(cmd *cobra.Command, args []string) {
	idOrName := cronJobID
	if idOrName == "" {
		idOrName = cronName
	}
	if idOrName == "" {
		fmt.Fprintln(os.Stderr, "Error: --id or --name is required")
		os.Exit(1)
	}
	port := getCronPort()
	data, _ := json.Marshal(struct {
		ID string `json:"id"`
	}{idOrName})
	url := buildAPIURL(cronHost, port, "/cron/remove")
	resp, err := apiRequest("POST", url, bytes.NewReader(data), cronToken)
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
	fmt.Println("Cron job removed.")
}

func runCronLog(cmd *cobra.Command, args []string) {
	idOrName := cronJobID
	if idOrName == "" {
		idOrName = cronName
	}
	if idOrName == "" {
		fmt.Fprintln(os.Stderr, "Error: --id or --name is required")
		os.Exit(1)
	}
	port := getCronPort()
	url := buildAPIURL(cronHost, port, "/cron/list")
	resp, err := apiRequest("GET", url, nil, cronToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot connect to bot: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result struct {
		Jobs []cronJob `json:"jobs"`
	}
	json.Unmarshal(body, &result)
	var job *cronJob
	for i, j := range result.Jobs {
		if j.ID == idOrName || j.Name == idOrName || (len(j.ID) >= 8 && j.ID[:8] == idOrName) {
			job = &result.Jobs[i]
			break
		}
	}
	if job == nil {
		fmt.Fprintf(os.Stderr, "Error: job '%s' not found\n", idOrName)
		os.Exit(1)
	}
	if job.Mode != "print" {
		fmt.Fprintln(os.Stderr, "Error: log is only available for print mode jobs")
		os.Exit(1)
	}
	if job.SessionID == "" {
		fmt.Fprintln(os.Stderr, "Error: job has no session yet (never executed)")
		os.Exit(1)
	}
	home, _ := os.UserHomeDir()
	slug := strings.ReplaceAll(job.CWD, "/", "-")
	slug = strings.ReplaceAll(slug, "_", "-")
	transcriptPath := filepath.Join(home, ".claude", "projects", slug, job.SessionID+".jsonl")
	data, err := os.ReadFile(transcriptPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot read transcript: %v\n", err)
		os.Exit(1)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry struct {
			Type    string `json:"type"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		if entry.Type != "user" && entry.Type != "assistant" {
			continue
		}
		var contentStr string
		if json.Unmarshal(entry.Message.Content, &contentStr) == nil && contentStr != "" {
			fmt.Printf("[%s] %s\n", entry.Type, contentStr)
			continue
		}
		var contentArr []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(entry.Message.Content, &contentArr) == nil {
			for _, c := range contentArr {
				if c.Type == "text" && c.Text != "" {
					fmt.Printf("[%s] %s\n", entry.Type, c.Text)
				}
			}
		}
	}
}

func runCronPause(cmd *cobra.Command, args []string) {
	id := cronJobID
	if id == "" {
		id = cronName
	}
	if id == "" {
		fmt.Fprintln(os.Stderr, "Error: --id or --name is required")
		os.Exit(1)
	}
	port := getCronPort()
	reqBody := struct {
		ID      string            `json:"id"`
		Updates map[string]string `json:"updates"`
	}{id, map[string]string{"paused": "true"}}
	data, _ := json.Marshal(reqBody)
	url := buildAPIURL(cronHost, port, "/cron/update")
	resp, err := apiRequest("POST", url, bytes.NewReader(data), cronToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "Error: %s\n", string(body))
		os.Exit(1)
	}
	fmt.Printf("Job %s paused.\n", id)
}

func runCronResume(cmd *cobra.Command, args []string) {
	id := cronJobID
	if id == "" {
		id = cronName
	}
	if id == "" {
		fmt.Fprintln(os.Stderr, "Error: --id or --name is required")
		os.Exit(1)
	}
	port := getCronPort()
	reqBody := struct {
		ID      string            `json:"id"`
		Updates map[string]string `json:"updates"`
	}{id, map[string]string{"paused": "false"}}
	data, _ := json.Marshal(reqBody)
	url := buildAPIURL(cronHost, port, "/cron/update")
	resp, err := apiRequest("POST", url, bytes.NewReader(data), cronToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "Error: %s\n", string(body))
		os.Exit(1)
	}
	fmt.Printf("Job %s resumed.\n", id)
}

func runCronUpdate(cmd *cobra.Command, args []string) {
	if cronJobID == "" {
		fmt.Fprintln(os.Stderr, "Error: --id is required (job ID or name)")
		os.Exit(1)
	}
	port := getCronPort()
	updates := make(map[string]string)
	if cmd.Flags().Changed("prompt") {
		updates["prompt"] = cronPrompt
	}
	if cmd.Flags().Changed("schedule") {
		updates["schedule"] = cronSchedule
	}
	if cmd.Flags().Changed("agent") {
		updates["agent_name"] = cronAgent
	}
	if cmd.Flags().Changed("name") {
		updates["name"] = cronName
	}
	if len(updates) == 0 {
		fmt.Fprintln(os.Stderr, "Error: no fields to update (use --prompt, --schedule, --agent, or --name)")
		os.Exit(1)
	}
	reqBody := struct {
		ID      string            `json:"id"`
		Updates map[string]string `json:"updates"`
	}{cronJobID, updates}
	data, _ := json.Marshal(reqBody)
	url := buildAPIURL(cronHost, port, "/cron/update")
	resp, err := apiRequest("POST", url, bytes.NewReader(data), cronToken)
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
	fmt.Println("Cron job updated.")
}
