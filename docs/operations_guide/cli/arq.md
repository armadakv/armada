---
title: "arq"
section: "operations_guide"
subsection: "cli"
order: 2
---
# NAME

arq - arq is the Armada query and data-plane CLI.

# SYNOPSIS

arq

```
[--address]=[value]
[--ca]=[value]
[--cert]=[value]
[--command-timeout]=[value]
[--help|-h]
[--insecure-skip-verify]
[--key]=[value]
[--server-name]=[value]
[--table|-t]=[value]
[--token]=[value]
[--write-out|-w]=[value]
```

# DESCRIPTION

arq reads and modifies key-value data stored in Armada tables.

**Usage**:

```
arq [GLOBAL OPTIONS] [command [COMMAND OPTIONS]] [ARGUMENTS...]
```

# GLOBAL OPTIONS

**--address**="": Armada API address. The scheme selects the transport (http/https/unix/unixs). (default: http://127.0.0.1:8443)

**--ca**="": Path to the CA certificate used to verify the server (TLS addresses only).

**--cert**="": Path to the client certificate to present for mutual TLS (TLS addresses only).

**--command-timeout**="": Timeout for an Armada API request; zero disables the timeout. (default: 30s)

**--help, -h**: show help (default: false)

**--insecure-skip-verify**: Skips verification of the server certificate chain and host name (TLS addresses only). (default: false)

**--key**="": Path to the client private key for mutual TLS (TLS addresses only).

**--server-name**="": Overrides the server name used to verify the server certificate (TLS addresses only).

**--table, -t**="": Armada table on which to operate.

**--token**="": The access token to use for authentication.

**--write-out, -w**="": Output format: simple or json. (default: simple)


# COMMANDS

## get, query

Get a key or range of keys.

**--all**: Get all keys in the table; no key argument is accepted. (default: false)

**--count-only**: Return only the number of matching keys. (default: false)

**--from-key**: Get all keys greater than or equal to the supplied key. (default: false)

**--help, -h**: show help (default: false)

**--keys-only**: Return keys without values. (default: false)

**--limit**="": Maximum number of keys to return; zero means no limit. (default: 0)

**--linearizable**: Use a linearizable read instead of the default serializable read. (default: false)

**--prefix**: Get all keys with the supplied key prefix. (default: false)

**--stream**: Stream range results for large result sets. (default: false)

**--value-only**: Print values without keys in simple output. (default: false)

### help, h

Shows a list of commands or help for one command

## put

Put a key-value pair.

**--help, -h**: show help (default: false)

**--prev-kv**: Return the key's previous value, if present. (default: false)

### help, h

Shows a list of commands or help for one command

## delete, del, rm

Delete a key or range of keys.

**--all**: Delete all keys in the table; no key argument is accepted. (default: false)

**--count**: Count and return the number of deleted keys. (default: false)

**--from-key**: Delete all keys greater than or equal to the supplied key. (default: false)

**--help, -h**: show help (default: false)

**--prefix**: Delete all keys with the supplied key prefix. (default: false)

**--prev-kv**: Return deleted key-value pairs. (default: false)

### help, h

Shows a list of commands or help for one command

## txn

Execute an atomic transaction.

**--file, -f**="": Read the transaction from a file instead of stdin; use - for stdin.

**--help, -h**: show help (default: false)

### help, h

Shows a list of commands or help for one command

## watch

Watch a key or range of keys for changes.

**--all**: Watch all keys in the table; no key argument is accepted. (default: false)

**--from-key**: Watch all keys greater than or equal to the supplied key. (default: false)

**--help, -h**: show help (default: false)

**--no-delete**: Do not print DELETE events. (default: false)

**--no-put**: Do not print PUT events. (default: false)

**--prefix**: Watch all keys with the supplied key prefix. (default: false)

**--prev-kv**: Print the previous key-value pair of each event, when it is still available. (default: false)

**--progress-notify**: Ask for periodic progress notifications (printed with --write-out=json). (default: false)

**--rev**="": Revision to start watching from; zero means changes after now. (default: 0)

### help, h

Shows a list of commands or help for one command

## version

Print current version.

**--help, -h**: show help (default: false)

### help, h

Shows a list of commands or help for one command

## help, h

Shows a list of commands or help for one command


# TRANSACTION INPUT

Transactions contain ordered `cmp`, `then`, and optional `else` sections:

```text
cmp
value("status") = "pending"
then
put "status" "ready"
get "status"
else
get "status"
```

Supported comparisons are `value("KEY") = "VALUE"` and the `==`, `!=`, `>`, and `<` variants.
Supported operations are `get KEY [RANGE_END]`, `put KEY VALUE`, and `delete KEY [RANGE_END]`.
Quote keys and values containing whitespace. Lines beginning with `#` and blank lines are ignored.
