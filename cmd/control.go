// Copyright JAMF Software, LLC

package cmd

import (
	"crypto/tls"
	"crypto/x509"
	"os"

	rl "github.com/armadakv/armada/log"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func init() {
	addControlClientFlags(arctlRootCmd)
}

func addControlClientFlags(cmd *cobra.Command) {
	cmd.PersistentFlags().String("address", "127.0.0.1:8445", "Armada maintenance API address.")
	cmd.PersistentFlags().String("ca", "", "Path to the client CA certificate.")
	cmd.PersistentFlags().String("token", "", "The access token to use for the authentication.")
	cmd.PersistentFlags().Bool("json", false, "Enables JSON logging.")
}

func newMaintenanceClientConn() (*grpc.ClientConn, error) {
	var cp *x509.CertPool
	ca := viper.GetString("ca")
	if ca != "" {
		caBytes, err := os.ReadFile(ca)
		if err != nil {
			return nil, err
		}
		cp = x509.NewCertPool()
		cp.AppendCertsFromPEM(caBytes)
	}

	creds := credentials.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    cp,
	})
	return grpc.NewClient(viper.GetString("address"), grpc.WithTransportCredentials(creds), grpc.WithPerRPCCredentials(tokenCredentials(viper.GetString("token"))))
}

func newJSONLogger() *zap.SugaredLogger {
	l := rl.NewLogger(false, zap.InfoLevel.String())
	return l.Sugar()
}
