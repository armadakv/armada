// Copyright Armada Contributors

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	clidocs "github.com/urfave/cli-docs/v3"
	"github.com/urfave/cli/v3"
)

var docsCmd = &cli.Command{
	Name:   "docs",
	Usage:  "Generate arq CLI documentation.",
	Hidden: true,
	Flags: []cli.Flag{
		&cli.StringFlag{Name: "destination", Value: "docs", Usage: "Destination folder where CLI docs should be generated."},
	},
	Action: func(ctx context.Context, c *cli.Command) error {
		destination := c.String("destination")
		if err := os.MkdirAll(destination, 0o755); err != nil {
			return err
		}

		markdown, err := clidocs.ToMarkdown(c.Root())
		if err != nil {
			return err
		}
		contents := "---\ntitle: \"arq\"\nsection: \"operations_guide\"\nsubsection: \"cli\"\norder: 2\n---\n" + markdown + txnDocumentation
		if err := os.WriteFile(filepath.Join(destination, "arq.md"), []byte(contents), 0o600); err != nil {
			return err
		}
		fmt.Printf("docs generated in '%s'\n", destination)
		return nil
	},
}
