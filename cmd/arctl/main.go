// Copyright Armada Contributors

package main

import (
	"log"
	"os"
	"strings"

	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/v2"
	"github.com/urfave/cli/v2"
)

var k = koanf.New(".")

func main() {
	app := &cli.App{
		Name:        "arctl",
		Usage:       "arctl is the Armada control-plane and maintenance CLI.",
		Description: "arctl provides backup, restore, and other cluster administration workflows for Armada.",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "address",
				Value: "127.0.0.1:8445",
				Usage: "Armada maintenance API address.",
			},
			&cli.StringFlag{
				Name:  "ca",
				Usage: "Path to the client CA certificate.",
			},
			&cli.StringFlag{
				Name:  "token",
				Usage: "The access token to use for authentication.",
			},
			&cli.BoolFlag{
				Name:  "json",
				Usage: "Enables JSON logging.",
			},
		},
		Before: func(c *cli.Context) error {
			// Load environment variables
			if err := k.Load(env.Provider("", ".", func(s string) string {
				return strings.ReplaceAll(strings.ToLower(s), "_", ".")
			}), nil); err != nil {
				return err
			}

			// Load flags manually into koanf via confmap
			flagsMap := make(map[string]interface{})
			for _, flagName := range c.FlagNames() {
				if c.IsSet(flagName) {
					flagsMap[flagName] = c.Value(flagName)
				}
			}
			if err := k.Load(confmap.Provider(flagsMap, "."), nil); err != nil {
				return err
			}

			return nil
		},
		Commands: []*cli.Command{
			backupCmd,
			restoreCmd,
			versionCmd,
			docsCmd,
		},
	}

	if err := app.Run(os.Args); err != nil {
		log.Fatal(err)
	}
}
