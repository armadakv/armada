// Copyright Armada Contributors

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/armadakv/armada/armadapb"
	"github.com/urfave/cli/v3"
)

type watchOptions struct {
	prefix         bool
	fromKey        bool
	all            bool
	rev            int64
	prevKV         bool
	progressNotify bool
	noPut          bool
	noDelete       bool
}

var watchCmd = &cli.Command{
	Name:      "watch",
	Usage:     "Watch a key or range of keys for changes.",
	ArgsUsage: "<key> [range-end]",
	Description: `Watch a key or the right-open key range [key, range-end) of the configured table and
print every change until interrupted.

Use --prefix for all keys sharing a prefix, --from-key for all keys greater than or equal to key,
or --all to watch the complete table. --rev starts at a past revision and replays the history from
there; without it only changes after the watch was created are printed.

The simple output prints the event type, the previous key and value when --prev-kv is set, and
then the key and value. With --write-out=json, each response is emitted as one JSON object per line,
including progress notifications. The watch ignores --command-timeout.`,
	Flags: []cli.Flag{
		&cli.BoolFlag{Name: "prefix", Usage: "Watch all keys with the supplied key prefix."},
		&cli.BoolFlag{Name: "from-key", Usage: "Watch all keys greater than or equal to the supplied key."},
		&cli.BoolFlag{Name: "all", Usage: "Watch all keys in the table; no key argument is accepted."},
		&cli.Int64Flag{Name: "rev", Usage: "Revision to start watching from; zero means changes after now."},
		&cli.BoolFlag{Name: "prev-kv", Usage: "Print the previous key-value pair of each event, when it is still available."},
		&cli.BoolFlag{Name: "progress-notify", Usage: "Ask for periodic progress notifications (printed with --write-out=json)."},
		&cli.BoolFlag{Name: "no-put", Usage: "Do not print PUT events."},
		&cli.BoolFlag{Name: "no-delete", Usage: "Do not print DELETE events."},
	},
	Action: func(ctx context.Context, c *cli.Command) error {
		opts := watchOptions{
			prefix:         c.Bool("prefix"),
			fromKey:        c.Bool("from-key"),
			all:            c.Bool("all"),
			rev:            c.Int64("rev"),
			prevKV:         c.Bool("prev-kv"),
			progressNotify: c.Bool("progress-notify"),
			noPut:          c.Bool("no-put"),
			noDelete:       c.Bool("no-delete"),
		}
		request, err := buildWatchRequest(k.String("table"), c.Args().Slice(), opts)
		if err != nil {
			return cli.Exit(err.Error(), 1)
		}
		conn, err := dial()
		if err != nil {
			return err
		}
		defer conn.Close()

		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
		stream, err := armadapb.NewWatchClient(conn).Watch(ctx)
		if err != nil {
			return err
		}
		if err := stream.Send(&armadapb.WatchRequest{RequestUnion: &armadapb.WatchRequest_CreateRequest{CreateRequest: request}}); err != nil {
			return err
		}
		err = runWatch(stream, os.Stdout, opts.prevKV)
		if ctx.Err() != nil {
			return nil
		}
		return err
	},
}

func buildWatchRequest(table string, args []string, opts watchOptions) (*armadapb.WatchCreateRequest, error) {
	if table == "" {
		return nil, fmt.Errorf("--table must be provided")
	}
	if opts.rev < 0 {
		return nil, fmt.Errorf("--rev must not be negative")
	}
	if opts.noPut && opts.noDelete {
		return nil, fmt.Errorf("--no-put and --no-delete together filter out every event")
	}
	rangeModes := 0
	for _, enabled := range []bool{opts.prefix, opts.fromKey, opts.all} {
		if enabled {
			rangeModes++
		}
	}
	if rangeModes > 1 {
		return nil, fmt.Errorf("only one of --prefix, --from-key, and --all may be used")
	}

	var key, rangeEnd []byte
	if opts.all {
		if len(args) != 0 {
			return nil, fmt.Errorf("--all does not accept key arguments")
		}
		key = []byte{0}
		rangeEnd = []byte{0}
	} else {
		if len(args) < 1 || args[0] == "" {
			return nil, fmt.Errorf("key must be provided")
		}
		if len(args) > 2 {
			return nil, fmt.Errorf("watch accepts at most a key and range-end")
		}
		key = []byte(args[0])
		if len(args) == 2 {
			if opts.prefix || opts.fromKey {
				return nil, fmt.Errorf("an explicit range-end cannot be used with --prefix or --from-key")
			}
			rangeEnd = []byte(args[1])
		} else if opts.prefix {
			rangeEnd = prefixRangeEnd(key)
		} else if opts.fromKey {
			rangeEnd = []byte{0}
		}
	}

	req := &armadapb.WatchCreateRequest{
		Table:          []byte(table),
		Key:            key,
		RangeEnd:       rangeEnd,
		StartRevision:  opts.rev,
		PrevKv:         opts.prevKV,
		ProgressNotify: opts.progressNotify,
		// Fragments are reassembled by runWatch, so large revisions never
		// cancel the watch.
		Fragment: true,
	}
	if opts.noPut {
		req.Filters = append(req.Filters, armadapb.WatchCreateRequest_NOPUT)
	}
	if opts.noDelete {
		req.Filters = append(req.Filters, armadapb.WatchCreateRequest_NODELETE)
	}
	return req, nil
}

type watchStream interface {
	Recv() (*armadapb.WatchResponse, error)
}

// runWatch prints responses until the watch ends. A watch canceled by the
// server is reported as an error.
func runWatch(stream watchStream, w io.Writer, prevKV bool) error {
	var pending *armadapb.WatchResponse
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if resp.GetCanceled() {
			return watchCanceled(resp)
		}
		if resp.GetFragment() {
			if pending == nil {
				pending = resp
			} else {
				pending.Events = append(pending.Events, resp.GetEvents()...)
			}
			continue
		}
		if pending != nil {
			pending.Events = append(pending.Events, resp.GetEvents()...)
			pending.Fragment = false
			resp, pending = pending, nil
		}
		if err := writeWatchResponse(w, resp, prevKV); err != nil {
			return err
		}
	}
}

func watchCanceled(resp *armadapb.WatchResponse) error {
	msg := "watch canceled"
	if reason := resp.GetCancelReason(); reason != "" {
		msg += ": " + reason
	}
	if rev := resp.GetCompactRevision(); rev > 0 {
		msg += fmt.Sprintf(" (first available revision %d)", rev)
	}
	return cli.Exit(msg, 1)
}

func writeWatchResponse(w io.Writer, resp *armadapb.WatchResponse, prevKV bool) error {
	if k.String("write-out") == "json" {
		return writeJSON(w, resp)
	}
	for _, ev := range resp.GetEvents() {
		if _, err := fmt.Fprintln(w, ev.GetType().String()); err != nil {
			return err
		}
		if prevKV && ev.GetPrevKv() != nil {
			if err := writePreviousKVs(w, []*armadapb.KeyValue{ev.GetPrevKv()}); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(w, string(ev.GetKv().GetKey())); err != nil {
			return err
		}
		if ev.GetType() == armadapb.Event_PUT {
			if _, err := fmt.Fprintln(w, string(ev.GetKv().GetValue())); err != nil {
				return err
			}
		}
	}
	return nil
}
