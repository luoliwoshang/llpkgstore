package internal

import (
	"github.com/luoliwoshang/llpkgstore/internal/actions"
	"github.com/spf13/cobra"
)

var issueCloseCmd = &cobra.Command{
	Use:   "issueclose",
	Short: "Legacy version maintenance on label creating",
	Long:  ``,

	Run: runIssueCloseCmd,
}

func runIssueCloseCmd(cmd *cobra.Command, args []string) {
	actions.NewDefaultClient().CleanResource()
}

func init() {
	rootCmd.AddCommand(issueCloseCmd)
}
