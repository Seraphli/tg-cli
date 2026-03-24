package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var UsageCmd = &cobra.Command{
	Use:   "usage",
	Short: "Show current Claude API usage stats",
	Run: func(cmd *cobra.Command, args []string) {
		formatted, err := fetchUsageFormatted()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(formatted)
	},
}
