package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

var ReloadCmd = &cobra.Command{
	Use:   "reload",
	Short: "Make the running bot re-read routes.json and credentials.json",
	Long:  "Asks the running bot to re-read routes.json and credentials.json (after a hand edit) and prints the counts. A file that does not parse leaves the bot as it was. The bot token, the port and the API token are read at startup only; changing them needs a restart.",
	Run:   runReload,
}

func init() {
	ReloadCmd.Flags().StringVar(&routeHost, "host", "", "Bot API host URL")
	ReloadCmd.Flags().StringVar(&routeToken, "token", "", "API authentication token")
	ReloadCmd.Flags().IntVar(&routePort, "port", 0, "Bot HTTP port (default: from config or 12500)")
}

// requestReload posts to the running bot's /reload and returns the sentence to print.
func requestReload(host string, port int, token string) (string, error) {
	resp, err := apiRequest("POST", buildAPIURL(host, port, "/reload"), nil, token)
	if err != nil {
		return "", fmt.Errorf("cannot connect to bot: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", errors.New(strings.TrimSpace(string(body)))
	}
	var res struct {
		Counts string `json:"counts"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return "", fmt.Errorf("unexpected response: %v", err)
	}
	return "Reloaded: " + res.Counts, nil
}

func runReload(cmd *cobra.Command, args []string) {
	out, err := requestReload(routeHost, getRoutePort(), routeToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(out)
}
