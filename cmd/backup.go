// Copyright JAMF Software, LLC

package cmd

import (
	"crypto/tls"
	"crypto/x509"
	"os"

	rl "github.com/armadakv/armada/log"
	"github.com/armadakv/armada/replication/backup"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func init() {
	backupCmd.PersistentFlags().String("address", "127.0.0.1:8445", "Armada maintenance API address.")
	backupCmd.PersistentFlags().String("dir", "", "Target directory (current directory if empty).")
	backupCmd.PersistentFlags().String("ca", "", "Path to the client CA certificate.")
	backupCmd.PersistentFlags().String("token", "", "The access token to use for the authentication.")
	backupCmd.PersistentFlags().Bool("json", false, "Enables JSON logging.")
}

var backupCmd = &cobra.Command{
	Use:   "backup",
	Short: "Backup Armada to local files.",
	Long: `Command backs up Armada into a directory of choice. All tables present in the target server are backed up.
Backup consists of file per a table in a binary compressed form and a human-readable manifest file. Use restore command to load backup into the server.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		var cp *x509.CertPool
		ca := viper.GetString("ca")
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
		conn, err := grpc.NewClient(viper.GetString("address"), grpc.WithTransportCredentials(creds), grpc.WithPerRPCCredentials(tokenCredentials(viper.GetString("token"))))
		if err != nil {
			return err
		}

		b := backup.Backup{
			Conn: conn,
			Dir:  viper.GetString("dir"),
		}
		if viper.GetBool("json") {
			l := rl.NewLogger(false, zap.InfoLevel.String())
			b.Log = l.Sugar()
		}
		_, err = b.Backup()
		if err != nil {
			b.Log.Infof("backup failed: %v", err)
		}
		return err
	},
	PreRunE: func(cmd *cobra.Command, args []string) error {
		initConfig(cmd.PersistentFlags())
		return nil
	},
	DisableAutoGenTag: true,
}
