// Copyright Armada Contributors

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/urfave/cli/v3"
)

var app = &cli.Command{
	Name:  "armada",
	Usage: "Armada is a read-optimized distributed key-value store.",
	Description: `Armada can be run in two modes -- leader and follower. Write API is enabled in the leader mode
and the node (or cluster of leader nodes) acts as a source of truth for the follower nodes/clusters.
Write API is disabled in the follower mode and the follower node or cluster of follower nodes replicate the writes
done to the leader cluster to which the follower is connected to.`,
	Commands: []*cli.Command{
		leaderCmd,
		followerCmd,
		docsCmd,
		versionCmd,
	},
}

func main() {
	if err := app.Run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
