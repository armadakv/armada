// Copyright JAMF Software, LLC

package cmd

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestArmadaRootCommands(t *testing.T) {
	commands := childCommandUses(rootCmd)

	require.Contains(t, commands, "leader")
	require.Contains(t, commands, "follower")
	require.Contains(t, commands, "docs")
	require.Contains(t, commands, "version")
	require.NotContains(t, commands, "backup")
	require.NotContains(t, commands, "restore")
}

func TestArctlRootCommands(t *testing.T) {
	commands := childCommandUses(arctlRootCmd)

	require.Contains(t, commands, "backup")
	require.Contains(t, commands, "restore")
	require.NotContains(t, commands, "leader")
	require.NotContains(t, commands, "follower")
}

func childCommandUses(root *cobra.Command) []string {
	commands := make([]string, 0, len(root.Commands()))
	for _, command := range root.Commands() {
		commands = append(commands, command.Use)
	}
	return commands
}
