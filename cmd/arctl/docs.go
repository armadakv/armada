// Copyright Armada Contributors

package main

import (
	"fmt"
	"os"

	"github.com/urfave/cli/v2"
)

var docsCmd = &cli.Command{
	Name:   "docs",
	Usage:  "Generate arctl CLI documentation.",
	Hidden: true,
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "destination",
			Value: "docs",
			Usage: "Destination folder where CLI docs should be generated.",
		},
	},
	Action: func(c *cli.Context) error {
		docsDest := c.String("destination")
		err := os.MkdirAll(docsDest, 0o777)
		if err != nil {
			return err
		}

		// App is passed from the context
		res, err := c.App.ToMarkdown()
		if err != nil {
			return err
		}

		f, err := os.Create(docsDest + "/arctl.md")
		if err != nil {
			return err
		}
		defer f.Close()

		_, err = f.WriteString("---\ntitle: \"arctl\"\nsection: \"operations_guide\"\nsubsection: \"cli\"\norder: 1\n---\n")
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
