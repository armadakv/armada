// Copyright JAMF Software, LLC

package cmd

import (
	"github.com/armadakv/armada/replication/backup"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func init() {
	backupCmd.PersistentFlags().String("dir", "", "Target directory (current directory if empty).")
}

var backupCmd = &cobra.Command{
	Use:   "backup",
	Short: "Backup Armada to local files.",
	Long: `Command backs up Armada into a directory of choice. All tables present in the target server are backed up.
Backup consists of file per a table in a binary compressed form and a human-readable manifest file. Use restore command to load backup into the server.`,
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
		_, err = b.Backup()
		if err != nil {
			b.Log.Infof("backup failed: %v", err)
		}
		return err
	},
	PreRunE: func(cmd *cobra.Command, args []string) error {
		initConfig(cmd.Flags())
		return nil
	},
	DisableAutoGenTag: true,
}
