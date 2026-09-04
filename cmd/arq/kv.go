// Copyright Armada Contributors

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/armadakv/armada/armadapb"
	"github.com/urfave/cli/v3"
)

type getOptions struct {
	prefix       bool
	fromKey      bool
	all          bool
	limit        int64
	linearizable bool
	keysOnly     bool
	countOnly    bool
	stream       bool
	valueOnly    bool
}

var getCmd = &cli.Command{
	Name:      "get",
	Aliases:   []string{"query"},
	Usage:     "Get a key or range of keys.",
	ArgsUsage: "<key> [range-end]",
	Description: `Get a key or the right-open key range [key, range-end) from the configured table.

Use --prefix for all keys sharing a prefix, --from-key for all keys greater than or equal to key,
or --all to scan the complete table. --stream uses IterateRange and is suitable for large ranges.
With --write-out=json, streamed responses are emitted as one JSON object per line.`,
	Flags: []cli.Flag{
		&cli.BoolFlag{Name: "prefix", Usage: "Get all keys with the supplied key prefix."},
		&cli.BoolFlag{Name: "from-key", Usage: "Get all keys greater than or equal to the supplied key."},
		&cli.BoolFlag{Name: "all", Usage: "Get all keys in the table; no key argument is accepted."},
		&cli.Int64Flag{Name: "limit", Usage: "Maximum number of keys to return; zero means no limit."},
		&cli.BoolFlag{Name: "linearizable", Usage: "Use a linearizable read instead of the default serializable read."},
		&cli.BoolFlag{Name: "keys-only", Usage: "Return keys without values."},
		&cli.BoolFlag{Name: "count-only", Usage: "Return only the number of matching keys."},
		&cli.BoolFlag{Name: "stream", Usage: "Stream range results for large result sets."},
		&cli.BoolFlag{Name: "value-only", Usage: "Print values without keys in simple output."},
	},
	Action: func(ctx context.Context, c *cli.Command) error {
		opts := getOptions{
			prefix:       c.Bool("prefix"),
			fromKey:      c.Bool("from-key"),
			all:          c.Bool("all"),
			limit:        c.Int64("limit"),
			linearizable: c.Bool("linearizable"),
			keysOnly:     c.Bool("keys-only"),
			countOnly:    c.Bool("count-only"),
			stream:       c.Bool("stream"),
			valueOnly:    c.Bool("value-only"),
		}
		request, err := buildRangeRequest(k.String("table"), c.Args().Slice(), opts)
		if err != nil {
			return cli.Exit(err.Error(), 1)
		}
		conn, err := dial()
		if err != nil {
			return err
		}
		defer conn.Close()

		requestCtx, cancel := commandContext(ctx)
		defer cancel()
		client := armadapb.NewKVClient(conn)
		if opts.stream {
			stream, err := client.IterateRange(requestCtx, request)
			if err != nil {
				return err
			}
			for {
				response, err := stream.Recv()
				if errors.Is(err, io.EOF) {
					return nil
				}
				if err != nil {
					return err
				}
				if err := writeRange(os.Stdout, response, opts); err != nil {
					return err
				}
			}
		}

		response, err := client.Range(requestCtx, request)
		if err != nil {
			return err
		}
		return writeRange(os.Stdout, response, opts)
	},
}

func buildRangeRequest(table string, args []string, opts getOptions) (*armadapb.RangeRequest, error) {
	if table == "" {
		return nil, fmt.Errorf("--table must be provided")
	}
	if opts.limit < 0 {
		return nil, fmt.Errorf("--limit must not be negative")
	}
	if opts.keysOnly && opts.countOnly {
		return nil, fmt.Errorf("--keys-only and --count-only cannot be used together")
	}
	if opts.valueOnly && (opts.keysOnly || opts.countOnly) {
		return nil, fmt.Errorf("--value-only cannot be used with --keys-only or --count-only")
	}
	if opts.countOnly && opts.stream {
		return nil, fmt.Errorf("--count-only cannot be used with --stream")
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
			return nil, fmt.Errorf("get accepts at most a key and range-end")
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

	return &armadapb.RangeRequest{
		Table:        []byte(table),
		Key:          key,
		RangeEnd:     rangeEnd,
		Limit:        opts.limit,
		Linearizable: opts.linearizable,
		KeysOnly:     opts.keysOnly,
		CountOnly:    opts.countOnly,
	}, nil
}

func writeRange(w io.Writer, response *armadapb.RangeResponse, opts getOptions) error {
	if k.String("write-out") == "json" {
		return writeJSON(w, response)
	}
	return writeRangeSimple(w, response, opts.keysOnly, opts.countOnly, opts.valueOnly)
}

type deleteOptions struct {
	prefix  bool
	fromKey bool
	all     bool
	prevKV  bool
	count   bool
}

var putCmd = &cli.Command{
	Name:      "put",
	Usage:     "Put a key-value pair.",
	ArgsUsage: "<key> <value>",
	Flags: []cli.Flag{
		&cli.BoolFlag{Name: "prev-kv", Usage: "Return the key's previous value, if present."},
	},
	Action: func(ctx context.Context, c *cli.Command) error {
		args := c.Args().Slice()
		if k.String("table") == "" {
			return cli.Exit("--table must be provided", 1)
		}
		if len(args) != 2 || args[0] == "" {
			return cli.Exit("put requires a non-empty key and a value", 1)
		}
		conn, err := dial()
		if err != nil {
			return err
		}
		defer conn.Close()
		requestCtx, cancel := commandContext(ctx)
		defer cancel()
		response, err := armadapb.NewKVClient(conn).Put(requestCtx, &armadapb.PutRequest{
			Table:  []byte(k.String("table")),
			Key:    []byte(args[0]),
			Value:  []byte(args[1]),
			PrevKv: c.Bool("prev-kv"),
		})
		if err != nil {
			return err
		}
		if k.String("write-out") == "json" {
			return writeJSON(os.Stdout, response)
		}
		if _, err := fmt.Fprintln(os.Stdout, "OK"); err != nil {
			return err
		}
		if response.GetPrevKv() != nil {
			return writePreviousKVs(os.Stdout, []*armadapb.KeyValue{response.GetPrevKv()})
		}
		return nil
	},
}

var deleteCmd = &cli.Command{
	Name:      "delete",
	Aliases:   []string{"del", "rm"},
	Usage:     "Delete a key or range of keys.",
	ArgsUsage: "<key> [range-end]",
	Flags: []cli.Flag{
		&cli.BoolFlag{Name: "prefix", Usage: "Delete all keys with the supplied key prefix."},
		&cli.BoolFlag{Name: "from-key", Usage: "Delete all keys greater than or equal to the supplied key."},
		&cli.BoolFlag{Name: "all", Usage: "Delete all keys in the table; no key argument is accepted."},
		&cli.BoolFlag{Name: "prev-kv", Usage: "Return deleted key-value pairs."},
		&cli.BoolFlag{Name: "count", Usage: "Count and return the number of deleted keys."},
	},
	Action: func(ctx context.Context, c *cli.Command) error {
		opts := deleteOptions{prefix: c.Bool("prefix"), fromKey: c.Bool("from-key"), all: c.Bool("all"), prevKV: c.Bool("prev-kv"), count: c.Bool("count")}
		request, err := buildDeleteRequest(k.String("table"), c.Args().Slice(), opts)
		if err != nil {
			return cli.Exit(err.Error(), 1)
		}
		conn, err := dial()
		if err != nil {
			return err
		}
		defer conn.Close()
		requestCtx, cancel := commandContext(ctx)
		defer cancel()
		response, err := armadapb.NewKVClient(conn).DeleteRange(requestCtx, request)
		if err != nil {
			return err
		}
		if k.String("write-out") == "json" {
			return writeJSON(os.Stdout, response)
		}
		if err := writePreviousKVs(os.Stdout, response.GetPrevKvs()); err != nil {
			return err
		}
		if opts.count {
			_, err = fmt.Fprintln(os.Stdout, response.GetDeleted())
		} else {
			_, err = fmt.Fprintln(os.Stdout, "OK")
		}
		return err
	},
}

func buildDeleteRequest(table string, args []string, opts deleteOptions) (*armadapb.DeleteRangeRequest, error) {
	if table == "" {
		return nil, fmt.Errorf("--table must be provided")
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
	} else if len(args) < 1 || args[0] == "" {
		return nil, fmt.Errorf("key must be provided")
	} else if len(args) > 2 {
		return nil, fmt.Errorf("delete accepts at most a key and range-end")
	} else if len(args) == 2 {
		key = []byte(args[0])
		if opts.prefix || opts.fromKey {
			return nil, fmt.Errorf("an explicit range-end cannot be used with --prefix or --from-key")
		}
		rangeEnd = []byte(args[1])
	} else {
		key = []byte(args[0])
		if opts.prefix {
			rangeEnd = prefixRangeEnd(key)
		} else if opts.fromKey {
			rangeEnd = []byte{0}
		}
	}
	return &armadapb.DeleteRangeRequest{
		Table:    []byte(table),
		Key:      key,
		RangeEnd: rangeEnd,
		PrevKv:   opts.prevKV,
		Count:    opts.count,
	}, nil
}

func prefixRangeEnd(prefix []byte) []byte {
	end := append([]byte(nil), prefix...)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] < 0xff {
			end[i]++
			return end[:i+1]
		}
	}
	return []byte{0}
}

func commandContext(parent context.Context) (context.Context, context.CancelFunc) {
	timeout := k.Duration("command-timeout")
	if timeout <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, timeout)
}
