// Copyright Armada Contributors

package main

import (
	"crypto/tls"
	"crypto/x509"
	"os"

	rl "github.com/armadakv/armada/log"
	"github.com/armadakv/armada/replication/backup"
	"github.com/urfave/cli/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
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
	Action: func(c *cli.Context) error {
		// If dir is set on this command, we add it to koanf explicitly
		if c.IsSet("dir") {
			k.Set("dir", c.String("dir"))
		}

		var cp *x509.CertPool
		ca := k.String("ca")
		if ca != "" {
			caBytes, err := os.ReadFile(ca)
			if err != nil {
				return err
			}
			cp = x509.NewCertPool()
			cp.AppendCertsFromPEM(caBytes)
		}

		creds := credentials.NewTLS(&tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    cp,
		})

		conn, err := grpc.NewClient(k.String("address"), grpc.WithTransportCredentials(creds), grpc.WithPerRPCCredentials(tokenCredentials(k.String("token"))))
		if err != nil {
			return err
		}

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
