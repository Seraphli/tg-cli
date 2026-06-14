package cmd

import (
	"fmt"
	"os"

	"github.com/Seraphli/tg-cli/cmd/helpers"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/spf13/cobra"
)

var (
	usageMethod     string
	usageTmuxServer string
)

var UsageCmd = &cobra.Command{
	Use:   "usage",
	Short: "Show usage stats (Claude + Codex)",
	Run: func(cmd *cobra.Command, args []string) {
		switch usageMethod {
		case "":
			formatted, _, _, err := helpers.FetchMergedUsage(nil, nil)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
			fmt.Println(formatted)
		case "api":
			formatted, _, err := helpers.FetchUsageFormatted(nil)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
			fmt.Println(formatted)
		case "tmux":
			if usageTmuxServer != "" {
				injector.ServerName = usageTmuxServer
			}
			output, err := helpers.FetchUsageTmux()
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
			fmt.Println(output)
		default:
			fmt.Fprintf(os.Stderr, "Error: invalid --method %q (expected api, tmux, or omit for merged)\n", usageMethod)
			os.Exit(1)
		}
	},
}

func init() {
	UsageCmd.Flags().StringVar(&usageMethod, "method", "", "Usage retrieval method: api, tmux, or empty for merged (default: merged)")
	UsageCmd.Flags().StringVar(&usageTmuxServer, "tmux-server", "", "Tmux server name for tmux method (default: system default)")
}
