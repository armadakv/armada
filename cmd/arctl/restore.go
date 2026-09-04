// Copyright Armada Contributors

package main

import (
	"context"

	rl "github.com/armadakv/armada/log"
	"github.com/armadakv/armada/replication/backup"
	"github.com/urfave/cli/v3"
	"go.uber.org/zap"
)

var restoreCmd = &cli.Command{
	Name:  "restore",
	Usage: "Restore Armada from local files.",
	Description: `WARNING: Restoring from backup is a destructive operation and should be used only as part of break glass procedure.

Restore Armada cluster from a directory of choice. All tables present in the manifest.json will be restored.
Restoring is done sequentially, for the fine-grained control of what to restore use backup manifest file.
It is almost certain that after restore the cold-start of all the followers watching the restored leader cluster is going to be necessary.`,
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "dir",
			Usage: "Directory containing the backups (current directory if empty)",
		},
	},
	Action: func(ctx context.Context, c *cli.Command) error {
		if c.IsSet("dir") {
			k.Set("dir", c.String("dir"))
		}

		conn, err := dial()
		if err != nil {
			return err
		}
		defer conn.Close()

		b := backup.Backup{
			Conn: conn,
			Dir:  k.String("dir"),
		}
		if k.Bool("json") {
			l := rl.NewLogger(false, zap.InfoLevel.String())
			b.Log = l.Sugar()
		}
		return b.Restore()
	},
}
