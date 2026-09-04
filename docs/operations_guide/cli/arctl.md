---
title: "arctl"
section: "operations_guide"
subsection: "cli"
order: 3
---
# NAME

arctl - arctl is the Armada control-plane and maintenance CLI.

# SYNOPSIS

arctl

```
[--address]=[value]
[--ca]=[value]
[--cert]=[value]
[--help|-h]
[--insecure-skip-verify]
[--json]
[--key]=[value]
[--server-name]=[value]
[--token]=[value]
```

# DESCRIPTION

arctl provides backup, restore, and other cluster administration workflows for Armada.

**Usage**:

```
arctl [GLOBAL OPTIONS] [command [COMMAND OPTIONS]] [ARGUMENTS...]
```

# GLOBAL OPTIONS

**--address**="": Armada API address. The scheme selects the transport (http/https/unix/unixs). (default: "http://127.0.0.1:8443")

**--ca**="": Path to the CA certificate used to verify the server (TLS addresses only).

**--cert**="": Path to the client certificate to present for mutual TLS (TLS addresses only).

**--help, -h**: show help

**--insecure-skip-verify**: Skips verification of the server certificate chain and host name (TLS addresses only).

**--json**: Enables JSON logging.

**--key**="": Path to the client private key for mutual TLS (TLS addresses only).

**--server-name**="": Overrides the server name used to verify the server certificate (TLS addresses only).

**--token**="": The access token to use for authentication.


# COMMANDS

## backup

Backup Armada to local files.

**--dir**="": Target directory (current directory if empty).

**--help, -h**: show help

### help, h

Shows a list of commands or help for one command

## restore

Restore Armada from local files.

**--dir**="": Directory containing the backups (current directory if empty)

**--help, -h**: show help

### help, h

Shows a list of commands or help for one command

## tables

Manage Armada tables.

**--help, -h**: show help

### list, ls

List all tables present in the cluster.

**--format, -o**="": Output format: "table" or "json". (default: "table")

**--help, -h**: show help

#### help, h

Shows a list of commands or help for one command

### create

Create a table.

**--help, -h**: show help

#### help, h

Shows a list of commands or help for one command

### delete, rm

Delete a table.

**--help, -h**: show help

#### help, h

Shows a list of commands or help for one command

### help, h

Shows a list of commands or help for one command

## version

Print current version.

**--help, -h**: show help

### help, h

Shows a list of commands or help for one command

## help, h

Shows a list of commands or help for one command
