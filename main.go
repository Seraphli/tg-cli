package main

import (
	"fmt"
	"os"

	"github.com/Seraphli/tg-cli/cmd"
	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/spf13/cobra"
)

var version = "1.1.1"
var configDir string

func main() {
	rootCmd := &cobra.Command{
		Use:   "tg-cli",
		Short: "Telegram bot for remote control of Claude Code",
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			config.ConfigDir = configDir
		},
	}
	rootCmd.Version = version
	cmd.Version = version
	rootCmd.PersistentFlags().StringVar(&configDir, "config-dir", "", "Configuration directory (default ~/.tg-cli/)")
	rootCmd.AddCommand(cmd.BotCmd)
	rootCmd.AddCommand(cmd.HookCmd)
	rootCmd.AddCommand(cmd.SetupCmd)
	rootCmd.AddCommand(cmd.ServiceCmd)
	rootCmd.AddCommand(cmd.VoiceCmd)
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
