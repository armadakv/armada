// Copyright JAMF Software, LLC

package cmd

import (
	"github.com/armadakv/armada/replication/backup"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func init() {
	restoreCmd.PersistentFlags().String("dir", "", "Directory containing the backups (current directory if empty)")
}

var restoreCmd = &cobra.Command{
	Use:   "restore",
	Short: "Restore Armada from local files.",
	Long: `WARNING: Restoring from backup is a destructive operation and should be used only as part of break glass procedure.

Restore Armada cluster from a directory of choice. All tables present in the manifest.json will be restored.
Restoring is done sequentially, for the fine-grained control of what to restore use backup manifest file.
It is almost certain that after restore the cold-start of all the followers watching the restored leader cluster is going to be necessary.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		conn, err := newMaintenanceClientConn()
		if err != nil {
			return err
		}

		b := backup.Backup{
			Conn: conn,
			Dir:  viper.GetString("dir"),
		}
		if viper.GetBool("json") {
			b.Log = newJSONLogger()
		}
		return b.Restore()
	},
	PreRunE: func(cmd *cobra.Command, args []string) error {
		initConfig(cmd.InheritedFlags(), cmd.Flags())
		return nil
	},
	DisableAutoGenTag: true,
}
