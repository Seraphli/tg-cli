package cmd

import (
	"fmt"
	"os"

	"github.com/Seraphli/tg-cli/cmd/helpers"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/spf13/cobra"
)

var (
	usageMethod    string
	usageTmuxServer string
)

var UsageCmd = &cobra.Command{
	Use:   "usage",
	Short: "Show current Claude API usage stats",
	Run: func(cmd *cobra.Command, args []string) {
		switch usageMethod {
		case "api", "":
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
			fmt.Fprintf(os.Stderr, "Error: invalid --method %q (expected api or tmux)\n", usageMethod)
			os.Exit(1)
		}
	},
}

func init() {
	UsageCmd.Flags().StringVar(&usageMethod, "method", "api", "Usage retrieval method: api or tmux")
	UsageCmd.Flags().StringVar(&usageTmuxServer, "tmux-server", "", "Tmux server name for tmux method (default: system default)")
}
