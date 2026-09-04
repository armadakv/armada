// Copyright Armada Contributors

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/armadakv/armada/armadapb"
	"github.com/urfave/cli/v3"
)

var tablesCmd = &cli.Command{
	Name:  "tables",
	Usage: "Manage Armada tables.",
	Description: `Manage tables in an Armada cluster.

Tables can be created and deleted only on a leader cluster; followers replicate table
changes automatically. Listing tables is available on both leader and follower clusters.`,
	Commands: []*cli.Command{
		tablesListCmd,
		tablesCreateCmd,
		tablesDeleteCmd,
	},
}

var tablesListCmd = &cli.Command{
	Name:    "list",
	Aliases: []string{"ls"},
	Usage:   "List all tables present in the cluster.",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:    "format",
			Aliases: []string{"o"},
			Value:   "table",
			Usage:   "Output format: \"table\" or \"json\".",
		},
	},
	Action: func(ctx context.Context, c *cli.Command) error {
		format := c.String("format")
		switch format {
		case "table", "json":
		default:
			return cli.Exit(fmt.Sprintf("invalid --format %q: must be \"table\" or \"json\"", format), 1)
		}

		conn, err := dial()
		if err != nil {
			return err
		}
		defer conn.Close()

		resp, err := armadapb.NewTablesClient(conn).List(ctx, &armadapb.ListTablesRequest{})
		if err != nil {
			return err
		}

		if format == "json" {
			b, err := json.MarshalIndent(resp.GetTables(), "", "  ")
			if err != nil {
				return err
			}
			fmt.Println(string(b))
			return nil
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
		fmt.Fprintln(w, "NAME\tID")
		for _, t := range resp.GetTables() {
			fmt.Fprintf(w, "%s\t%s\n", t.GetName(), t.GetId())
		}
		return w.Flush()
	},
}

var tablesCreateCmd = &cli.Command{
	Name:      "create",
	Usage:     "Create a table.",
	ArgsUsage: "<name>",
	Description: `Create a table with the given name on the leader cluster. All followers will
automatically replicate the table.`,
	Action: func(ctx context.Context, c *cli.Command) error {
		name := c.Args().First()
		if name == "" {
			return cli.Exit("table name must be provided", 1)
		}

		conn, err := dial()
		if err != nil {
			return err
		}
		defer conn.Close()

		resp, err := armadapb.NewTablesClient(conn).Create(ctx, &armadapb.CreateTableRequest{Name: name})
		if err != nil {
			return err
		}

		fmt.Printf("table %q created with id %s\n", name, resp.GetId())
		return nil
	},
}

var tablesDeleteCmd = &cli.Command{
	Name:      "delete",
	Aliases:   []string{"rm"},
	Usage:     "Delete a table.",
	ArgsUsage: "<name>",
	Description: `Delete the table with the given name from the leader cluster. All followers will
automatically delete the table.`,
	Action: func(ctx context.Context, c *cli.Command) error {
		name := c.Args().First()
		if name == "" {
			return cli.Exit("table name must be provided", 1)
		}

		conn, err := dial()
		if err != nil {
			return err
		}
		defer conn.Close()

		if _, err := armadapb.NewTablesClient(conn).Delete(ctx, &armadapb.DeleteTableRequest{Name: name}); err != nil {
			return err
		}

		fmt.Printf("table %q deleted\n", name)
		return nil
	},
}
