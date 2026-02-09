package main

import (
	"fmt"
	"os"

	"github.com/Seraphli/tg-cli/cmd"
	"github.com/spf13/cobra"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "tg-cli",
		Short: "Telegram bot for remote control of Claude Code",
	}
	rootCmd.AddCommand(cmd.BotCmd)
	rootCmd.AddCommand(cmd.HookCmd)
	rootCmd.AddCommand(cmd.SetupCmd)
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
