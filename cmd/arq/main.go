// Copyright Armada Contributors

package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/urfave/cli/v3"
)

var app = &cli.Command{
	Name:        "arq",
	Usage:       "arq is the Armada query and data-plane CLI.",
	Description: "arq reads and modifies key-value data stored in Armada tables.",
	Flags: []cli.Flag{
		&cli.StringFlag{Name: "address", Value: "http://127.0.0.1:8443", Usage: "Armada API address. The scheme selects the transport (http/https/unix/unixs)."},
		&cli.StringFlag{Name: "ca", Usage: "Path to the CA certificate used to verify the server (TLS addresses only)."},
		&cli.StringFlag{Name: "cert", Usage: "Path to the client certificate to present for mutual TLS (TLS addresses only)."},
		&cli.StringFlag{Name: "key", Usage: "Path to the client private key for mutual TLS (TLS addresses only)."},
		&cli.StringFlag{Name: "server-name", Usage: "Overrides the server name used to verify the server certificate (TLS addresses only)."},
		&cli.BoolFlag{Name: "insecure-skip-verify", Usage: "Skips verification of the server certificate chain and host name (TLS addresses only)."},
		&cli.StringFlag{Name: "token", Usage: "The access token to use for authentication."},
		&cli.StringFlag{Name: "table", Aliases: []string{"t"}, Usage: "Armada table on which to operate."},
		&cli.StringFlag{Name: "write-out", Aliases: []string{"w"}, Value: "simple", Usage: "Output format: simple or json."},
		&cli.DurationFlag{Name: "command-timeout", Value: 30 * time.Second, Usage: "Timeout for an Armada API request; zero disables the timeout."},
	},
	Before: func(ctx context.Context, c *cli.Command) (context.Context, error) {
		if err := initConfig(c); err != nil {
			return ctx, err
		}
		switch k.String("write-out") {
		case "simple", "json":
		default:
			return ctx, fmt.Errorf("invalid --write-out %q: must be simple or json", k.String("write-out"))
		}
		timeout, err := configuredCommandTimeout()
		if err != nil {
			return ctx, err
		}
		if timeout < 0 {
			return ctx, fmt.Errorf("--command-timeout must not be negative")
		}
		k.Set("command-timeout", timeout)
		return ctx, nil
	},
	Commands: []*cli.Command{
		getCmd,
		putCmd,
		deleteCmd,
		txnCmd,
		versionCmd,
		docsCmd,
	},
}

func configuredCommandTimeout() (time.Duration, error) {
	value := k.Get("command-timeout")
	switch typed := value.(type) {
	case time.Duration:
		return typed, nil
	case string:
		duration, err := time.ParseDuration(typed)
		if err != nil {
			return 0, fmt.Errorf("invalid --command-timeout %q: %w", typed, err)
		}
		return duration, nil
	default:
		return 0, fmt.Errorf("invalid --command-timeout value %v", value)
	}
}

func main() {
	if err := app.Run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
