// Copyright Armada Contributors

package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/armadakv/armada/armadapb"
	"github.com/urfave/cli/v3"
)

var comparisonPattern = regexp.MustCompile(`^value\((.*)\)\s*(==|=|!=|>|<)\s*(.*)$`)

const txnDocumentation = `

# TRANSACTION INPUT

Transactions contain ordered ` + "`cmp`" + `, ` + "`then`" + `, and optional ` + "`else`" + ` sections:

` + "```text" + `
cmp
value("status") = "pending"
then
put "status" "ready"
get "status"
else
get "status"
` + "```" + `

Supported comparisons are ` + "`value(\"KEY\") = \"VALUE\"`" + ` and the ` + "`==`" + `, ` + "`!=`" + `, ` + "`>`" + `, and ` + "`<`" + ` variants.
Supported operations are ` + "`get KEY [RANGE_END]`" + `, ` + "`put KEY VALUE`" + `, and ` + "`delete KEY [RANGE_END]`" + `.
Quote keys and values containing whitespace. Lines beginning with ` + "`#`" + ` and blank lines are ignored.
`

var txnCmd = &cli.Command{
	Name:  "txn",
	Usage: "Execute an atomic transaction.",
	Description: `Read an etcd-style transaction from stdin or --file. Input is split into cmp, then,
and else sections. Each section starts with its name on a line by itself. Comparisons use
value("key") = "value" (also !=, >, and <). Operations use get KEY [RANGE_END],
put KEY VALUE, or delete KEY [RANGE_END]. Empty cmp and else sections are allowed.

Example:
  cmp
  value("status") = "pending"
  then
  put "status" "ready"
  get "status"
  else
  get "status"`,
	Flags: []cli.Flag{
		&cli.StringFlag{Name: "file", Aliases: []string{"f"}, Usage: "Read the transaction from a file instead of stdin; use - for stdin."},
	},
	Action: func(ctx context.Context, c *cli.Command) error {
		if k.String("table") == "" {
			return cli.Exit("--table must be provided", 1)
		}
		if c.Args().Len() != 0 {
			return cli.Exit("txn does not accept positional arguments; use --file", 1)
		}

		input := io.Reader(os.Stdin)
		var file *os.File
		if path := c.String("file"); path != "" && path != "-" {
			var err error
			file, err = os.Open(path)
			if err != nil {
				return err
			}
			defer file.Close()
			input = file
		} else if path == "" {
			if info, err := os.Stdin.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
				fmt.Fprintln(os.Stderr, "Enter cmp, then, and else sections; press Ctrl-D when finished:")
			}
		}

		request, err := parseTxn(input, k.String("table"))
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
		response, err := armadapb.NewKVClient(conn).Txn(requestCtx, request)
		if err != nil {
			return err
		}
		if k.String("write-out") == "json" {
			return writeJSON(os.Stdout, response)
		}
		return writeTxnSimple(os.Stdout, response)
	},
}

type txnSection uint8

const (
	txnSectionNone txnSection = iota
	txnSectionCompare
	txnSectionSuccess
	txnSectionFailure
)

func parseTxn(r io.Reader, table string) (*armadapb.TxnRequest, error) {
	request := &armadapb.TxnRequest{Table: []byte(table)}
	section := txnSectionNone
	seenCompare := false
	seenSuccess := false
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		switch strings.ToLower(line) {
		case "cmp", "compare":
			if seenCompare || seenSuccess {
				return nil, fmt.Errorf("line %d: cmp section is out of order", lineNumber)
			}
			seenCompare = true
			section = txnSectionCompare
			continue
		case "then", "success":
			if !seenCompare || seenSuccess {
				return nil, fmt.Errorf("line %d: then section is out of order", lineNumber)
			}
			seenSuccess = true
			section = txnSectionSuccess
			continue
		case "else", "failure":
			if !seenSuccess || section == txnSectionFailure {
				return nil, fmt.Errorf("line %d: else section is out of order", lineNumber)
			}
			section = txnSectionFailure
			continue
		}

		switch section {
		case txnSectionCompare:
			comparison, err := parseComparison(line)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNumber, err)
			}
			request.Compare = append(request.Compare, comparison)
		case txnSectionSuccess, txnSectionFailure:
			op, err := parseTxnOperation(line)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNumber, err)
			}
			if section == txnSectionSuccess {
				request.Success = append(request.Success, op)
			} else {
				request.Failure = append(request.Failure, op)
			}
		default:
			return nil, fmt.Errorf("line %d: expected cmp section", lineNumber)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if !seenCompare {
		return nil, fmt.Errorf("transaction is missing cmp section")
	}
	if !seenSuccess {
		return nil, fmt.Errorf("transaction is missing then section")
	}
	return request, nil
}

func parseComparison(line string) (*armadapb.Compare, error) {
	matches := comparisonPattern.FindStringSubmatch(line)
	if matches == nil {
		return nil, fmt.Errorf("invalid comparison %q", line)
	}
	key, err := parseSingleValue(matches[1])
	if err != nil || key == "" {
		return nil, fmt.Errorf("invalid comparison key")
	}
	value, err := parseSingleValue(matches[3])
	if err != nil {
		return nil, fmt.Errorf("invalid comparison value: %w", err)
	}

	var result armadapb.Compare_CompareResult
	switch matches[2] {
	case "=", "==":
		result = armadapb.Compare_EQUAL
	case "!=":
		result = armadapb.Compare_NOT_EQUAL
	case ">":
		result = armadapb.Compare_GREATER
	case "<":
		result = armadapb.Compare_LESS
	}
	return &armadapb.Compare{
		Result:      result,
		Target:      armadapb.Compare_VALUE,
		Key:         []byte(key),
		TargetUnion: &armadapb.Compare_Value{Value: []byte(value)},
	}, nil
}

func parseSingleValue(input string) (string, error) {
	words, err := splitWords(strings.TrimSpace(input))
	if err != nil {
		return "", err
	}
	if len(words) != 1 {
		return "", fmt.Errorf("expected one value")
	}
	return words[0], nil
}

func parseTxnOperation(line string) (*armadapb.RequestOp, error) {
	words, err := splitWords(line)
	if err != nil {
		return nil, err
	}
	if len(words) == 0 {
		return nil, fmt.Errorf("operation must not be empty")
	}
	switch strings.ToLower(words[0]) {
	case "get", "query":
		if len(words) < 2 || len(words) > 3 || words[1] == "" {
			return nil, fmt.Errorf("get requires KEY and optional RANGE_END")
		}
		op := &armadapb.RequestOp_Range{Key: []byte(words[1])}
		if len(words) == 3 {
			op.RangeEnd = []byte(words[2])
		}
		return &armadapb.RequestOp{Request: &armadapb.RequestOp_RequestRange{RequestRange: op}}, nil
	case "put":
		if len(words) != 3 || words[1] == "" {
			return nil, fmt.Errorf("put requires KEY and VALUE")
		}
		op := &armadapb.RequestOp_Put{Key: []byte(words[1]), Value: []byte(words[2])}
		return &armadapb.RequestOp{Request: &armadapb.RequestOp_RequestPut{RequestPut: op}}, nil
	case "delete", "del", "rm":
		if len(words) < 2 || len(words) > 3 || words[1] == "" {
			return nil, fmt.Errorf("delete requires KEY and optional RANGE_END")
		}
		op := &armadapb.RequestOp_DeleteRange{Key: []byte(words[1]), Count: true}
		if len(words) == 3 {
			op.RangeEnd = []byte(words[2])
		}
		return &armadapb.RequestOp{Request: &armadapb.RequestOp_RequestDeleteRange{RequestDeleteRange: op}}, nil
	default:
		return nil, fmt.Errorf("unsupported operation %q", words[0])
	}
}

func splitWords(input string) ([]string, error) {
	var words []string
	var word strings.Builder
	var quote rune
	escaped := false
	started := false
	flush := func() {
		if started {
			words = append(words, word.String())
			word.Reset()
			started = false
		}
	}
	for _, r := range input {
		if escaped {
			word.WriteRune(r)
			escaped = false
			started = true
			continue
		}
		if r == '\\' {
			escaped = true
			started = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			started = true
			continue
		}
		switch r {
		case '\'', '"':
			quote = r
			started = true
		case ' ', '\t', '\r', '\n':
			flush()
		default:
			word.WriteRune(r)
			started = true
		}
	}
	if escaped {
		return nil, fmt.Errorf("trailing escape")
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote")
	}
	flush()
	return words, nil
}

func writeTxnSimple(w io.Writer, response *armadapb.TxnResponse) error {
	if response.GetSucceeded() {
		if _, err := fmt.Fprintln(w, "SUCCESS"); err != nil {
			return err
		}
	} else if _, err := fmt.Fprintln(w, "FAILURE"); err != nil {
		return err
	}
	for _, op := range response.GetResponses() {
		if result := op.GetResponseRange(); result != nil {
			if err := writeRangeSimple(w, &armadapb.RangeResponse{Kvs: result.GetKvs(), More: result.GetMore(), Count: result.GetCount()}, false, false, false); err != nil {
				return err
			}
			continue
		}
		if result := op.GetResponsePut(); result != nil {
			if result.GetPrevKv() != nil {
				if err := writePreviousKVs(w, []*armadapb.KeyValue{result.GetPrevKv()}); err != nil {
					return err
				}
			}
			continue
		}
		if result := op.GetResponseDeleteRange(); result != nil {
			if err := writePreviousKVs(w, result.GetPrevKvs()); err != nil {
				return err
			}
			if _, err := fmt.Fprintln(w, result.GetDeleted()); err != nil {
				return err
			}
		}
	}
	return nil
}
