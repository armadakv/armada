// Copyright Armada Contributors

package main

import (
	"context"
	"fmt"
	"os"

	clidocs "github.com/urfave/cli-docs/v3"
	"github.com/urfave/cli/v3"
)

var docsCmd = &cli.Command{
	Name:   "docs",
	Usage:  "Generate Armada CLI documentation.",
	Hidden: true,
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "destination",
			Value: "docs",
			Usage: "Destination folder where CLI docs should be generated.",
		},
	},
	Action: func(ctx context.Context, c *cli.Command) error {
		docsDest := c.String("destination")
		err := os.MkdirAll(docsDest, 0o755)
		if err != nil {
			return err
		}

		res, err := clidocs.ToMarkdown(c.Root())
		if err != nil {
			return err
		}

		f, err := os.Create(docsDest + "/armada.md")
		if err != nil {
			return err
		}
		defer f.Close()

		_, err = f.WriteString("---\ntitle: \"armada\"\nsection: \"operations_guide\"\nsubsection: \"cli\"\norder: 1\n---\n")
		if err != nil {
			return err
		}

		_, err = f.WriteString(res)
		if err != nil {
			return err
		}

		fmt.Printf("docs generated in '%s'\n", docsDest)
		return nil
	},
}
