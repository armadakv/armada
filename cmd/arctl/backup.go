// Copyright Armada Contributors

package main

import (
	"context"

	rl "github.com/armadakv/armada/log"
	"github.com/armadakv/armada/replication/backup"
	"github.com/urfave/cli/v3"
	"go.uber.org/zap"
)

var backupCmd = &cli.Command{
	Name:  "backup",
	Usage: "Backup Armada to local files.",
	Description: `Command backs up Armada into a directory of choice. All tables present in the target server are backed up.
Backup consists of file per a table in a binary compressed form and a human-readable manifest file. Use restore command to load backup into the server.`,
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "dir",
			Usage: "Target directory (current directory if empty).",
		},
	},
	Action: func(ctx context.Context, c *cli.Command) error {
		// If dir is set on this command, we add it to koanf explicitly
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
		_, err = b.Backup()
		if err != nil {
			if b.Log != nil {
				b.Log.Infof("backup failed: %v", err)
			} else {
				rl.NewLogger(false, zap.InfoLevel.String()).Sugar().Infof("backup failed: %v", err)
			}
		}
		return err
	},
}
