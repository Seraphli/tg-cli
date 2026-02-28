package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"
)

var McpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Run MCP server for Claude Code",
	RunE:  runMcp,
}

var mcpPortFlag int

func init() {
	McpCmd.Flags().IntVar(&mcpPortFlag, "port", 0, "HTTP server port (overrides config)")
}

func runMcp(cmd *cobra.Command, args []string) error {
	creds, err := config.LoadCredentials()
	if err != nil {
		return fmt.Errorf("load credentials: %w", err)
	}
	port := mcpPortFlag
	if port == 0 {
		port = creds.Port
	}
	if port == 0 {
		port = 12500
	}
	cwd, _ := os.Getwd()

	s := server.NewMCPServer("tg-cli", Version)
	tool := mcp.NewTool("send_file",
		mcp.WithDescription("Send a file to Telegram chat"),
		mcp.WithString("file_path",
			mcp.Required(),
			mcp.Description("Absolute path to the file to send"),
		),
		mcp.WithString("caption",
			mcp.Description("Optional caption for the file"),
		),
	)
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		filePath, err := req.RequireString("file_path")
		if err != nil {
			return mcp.NewToolResultError("file_path is required"), nil
		}
		caption := req.GetString("caption", "")

		// Validate file exists and is regular
		info, err := os.Stat(filePath)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("File not found: %s", filePath)), nil
		}
		if !info.Mode().IsRegular() {
			return mcp.NewToolResultError(fmt.Sprintf("Not a regular file: %s", filePath)), nil
		}

		// POST to bot
		body, _ := json.Marshal(map[string]string{
			"file_path": filePath,
			"caption":   caption,
			"cwd":       cwd,
		})
		resp, err := http.Post(
			fmt.Sprintf("http://127.0.0.1:%d/mcp/send-file", port),
			"application/json",
			bytes.NewReader(body),
		)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Bot unreachable: %v", err)), nil
		}
		defer resp.Body.Close()

		var result struct {
			OK      bool   `json:"ok"`
			Message string `json:"message"`
			Error   string `json:"error"`
		}
		respBody, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return mcp.NewToolResultError(fmt.Sprintf("Bot returned HTTP %d: %s", resp.StatusCode, string(respBody))), nil
		}
		if err := json.Unmarshal(respBody, &result); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Invalid response from bot: %s", string(respBody))), nil
		}

		if !result.OK {
			return mcp.NewToolResultError(fmt.Sprintf("Send failed: %s", result.Error)), nil
		}
		return mcp.NewToolResultText(result.Message), nil
	})

	return server.ServeStdio(s)
}
