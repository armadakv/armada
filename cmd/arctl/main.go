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
		&cli.StringFlag{Name: "address", Value: "127.0.0.1:8445", Usage: "Armada maintenance API address."},
		&cli.StringFlag{Name: "ca", Usage: "Path to the client CA certificate."},
		&cli.StringFlag{Name: "token", Usage: "The access token to use for authentication."},
		&cli.BoolFlag{Name: "json", Usage: "Enables JSON logging."},
	},
	Before: func(ctx context.Context, c *cli.Command) (context.Context, error) {
		return ctx, initConfig(c)
	},
	Commands: []*cli.Command{
		backupCmd,
		restoreCmd,
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
