package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/spf13/cobra"
)

var RouteCmd = &cobra.Command{
	Use:   "route",
	Short: "Manage session routes",
}

var routeBindCmd = &cobra.Command{
	Use:   "bind",
	Short: "Bind a session name to a chat (a forum supergroup gives the session its own topic)",
	Run:   runRouteBind,
}

var (
	routeName  string
	routeChat  int64
	routePort  int
	routeHost  string
	routeToken string
)

func init() {
	RouteCmd.PersistentFlags().StringVar(&routeHost, "host", "", "Bot API host URL")
	RouteCmd.PersistentFlags().StringVar(&routeToken, "token", "", "API authentication token")
	routeBindCmd.Flags().StringVar(&routeName, "name", "", "Session name (required)")
	routeBindCmd.Flags().Int64Var(&routeChat, "chat", 0, "Chat id, negative for groups (required)")
	routeBindCmd.Flags().IntVar(&routePort, "port", 0, "Bot HTTP port (default: from config or 12500)")
	RouteCmd.AddCommand(routeBindCmd)
}

func getRoutePort() int {
	if routePort > 0 {
		return routePort
	}
	creds, err := config.LoadCredentials()
	if err == nil && creds.Port > 0 {
		return creds.Port
	}
	return 12500
}

// requestRouteBind posts to the running bot's /route/bind and returns the sentence to print. It sends no
// topic_id, so the bot applies the automatic rule (a forum supergroup gets the session's own topic).
func requestRouteBind(host string, port int, token, name string, chat int64) (string, error) {
	if name == "" || chat == 0 {
		return "", errors.New("--name and --chat are required")
	}
	data, _ := json.Marshal(struct {
		Name   string `json:"name"`
		ChatID int64  `json:"chat_id"`
	}{name, chat})
	resp, err := apiRequest("POST", buildAPIURL(host, port, "/route/bind"), bytes.NewReader(data), token)
	if err != nil {
		return "", fmt.Errorf("cannot connect to bot: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", errors.New(strings.TrimSpace(string(body)))
	}
	var res struct {
		TopicID int `json:"topic_id"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return "", fmt.Errorf("unexpected response: %v", err)
	}
	out := fmt.Sprintf("Route bound: %s → %d", name, chat)
	if res.TopicID != 0 {
		out += fmt.Sprintf(", topic=%d", res.TopicID)
	}
	return out, nil
}

func runRouteBind(cmd *cobra.Command, args []string) {
	out, err := requestRouteBind(routeHost, getRoutePort(), routeToken, routeName, routeChat)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(out)
}
