// Copyright JAMF Software, LLC

package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(leaderCmd)
	rootCmd.AddCommand(followerCmd)
	rootCmd.AddCommand(docsCmd)
	rootCmd.AddCommand(versionCmd)

	arctlRootCmd.AddCommand(backupCmd)
	arctlRootCmd.AddCommand(restoreCmd)
}

var rootCmd = &cobra.Command{
	Use:   "armada",
	Short: "Armada is a read-optimized distributed key-value store.",
	Long: `Armada can be run in two modes -- leader and follower. Write API is enabled in the leader mode
and the node (or cluster of leader nodes) acts as a source of truth for the follower nodes/clusters.
Write API is disabled in the follower mode and the follower node or cluster of follower nodes replicate the writes
done to the leader cluster to which the follower is connected to.`,
	Hidden:             true,
	SuggestFor:         []string{leaderCmd.Use, followerCmd.Use},
	DisableFlagParsing: true,
	DisableAutoGenTag:  true,
	CompletionOptions:  cobra.CompletionOptions{DisableDefaultCmd: true},
}

var arctlRootCmd = &cobra.Command{
	Use:   "arctl",
	Short: "Armada control CLI.",
	Long: `Arctl provides administrative and maintenance commands for Armada clusters,
including backup and restore workflows.`,
	Hidden:             true,
	DisableFlagParsing: true,
	DisableAutoGenTag:  true,
	CompletionOptions:  cobra.CompletionOptions{DisableDefaultCmd: true},
}

// Execute cobra command.
func Execute() {
	execute(rootCmd)
}

// ExecuteArctl executes the arctl cobra command.
func ExecuteArctl() {
	execute(arctlRootCmd)
}

func execute(root *cobra.Command) {
	if err := root.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
