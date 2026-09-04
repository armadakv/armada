// Copyright Armada Contributors

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/urfave/cli/v3"
)

var app = &cli.Command{
	Name:        "arctl",
	Usage:       "arctl is the Armada control-plane and maintenance CLI.",
	Description: "arctl provides backup, restore, and other cluster administration workflows for Armada.",
	Flags: []cli.Flag{
		&cli.StringFlag{Name: "address", Value: "http://127.0.0.1:8443", Usage: "Armada API address. The scheme selects the transport (http/https/unix/unixs)."},
		&cli.StringFlag{Name: "ca", Usage: "Path to the CA certificate used to verify the server (TLS addresses only)."},
		&cli.StringFlag{Name: "cert", Usage: "Path to the client certificate to present for mutual TLS (TLS addresses only)."},
		&cli.StringFlag{Name: "key", Usage: "Path to the client private key for mutual TLS (TLS addresses only)."},
		&cli.StringFlag{Name: "server-name", Usage: "Overrides the server name used to verify the server certificate (TLS addresses only)."},
		&cli.BoolFlag{Name: "insecure-skip-verify", Usage: "Skips verification of the server certificate chain and host name (TLS addresses only)."},
		&cli.StringFlag{Name: "token", Usage: "The access token to use for authentication."},
		&cli.BoolFlag{Name: "json", Usage: "Enables JSON logging."},
	},
	Before: func(ctx context.Context, c *cli.Command) (context.Context, error) {
		return ctx, initConfig(c)
	},
	Commands: []*cli.Command{
		backupCmd,
		restoreCmd,
		tablesCmd,
		versionCmd,
		docsCmd,
	},
}

func main() {
	if err := app.Run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
