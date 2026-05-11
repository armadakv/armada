// Copyright JAMF Software, LLC

package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"
)

var docsDest string

const frontMatterTemplate = `---
title: "%s"
section: "operations_guide"
subsection: "cli"
order: %d
---
`

// cliOrder defines the display order of each CLI command in the sidebar.
var cliOrder = map[string]int{
	"armada":          1,
	"armada_leader":   2,
	"armada_follower": 3,
	"armada_backup":   4,
	"armada_restore":  5,
	"armada_version":  6,
}

func init() {
	docsCmd.PersistentFlags().StringVar(&docsDest, "destination", "docs", "Destination folder where CLI docs should be generated.")
}

func identity(s string) string { return s }

func frontMatter(filename string) string {
	base := filepath.Base(filename)
	base = base[:len(base)-len(filepath.Ext(base))]
	command := strings.Join(strings.Split(base, "_"), " ")
	order, ok := cliOrder[base]
	if !ok {
		order = 99
	}
	return fmt.Sprintf(frontMatterTemplate, command, order)
}

var docsCmd = &cobra.Command{
	Use:                "docs",
	Short:              "Generate Armada CLI documentation.",
	Hidden:             true,
	DisableFlagParsing: false,
	RunE: func(cmd *cobra.Command, _ []string) error {
		// #nosec G301
		err := os.MkdirAll(docsDest, 0o777)
		if err != nil {
			return err
		}

		err = doc.GenMarkdownTreeCustom(rootCmd, docsDest, frontMatter, identity)
		if err != nil {
			return err
		}

		fmt.Printf("docs generated in '%s'\n", docsDest)
		return nil
	},
}
