---
title: "arctl"
section: "operations_guide"
subsection: "cli"
order: 1
---
# NAME

arctl - arctl is the Armada control-plane and maintenance CLI.

# SYNOPSIS

arctl

```
[--address]=[value]
[--ca]=[value]
[--help|-h]
[--json]
[--token]=[value]
```

# DESCRIPTION

arctl provides backup, restore, and other cluster administration workflows for Armada.

**Usage**:

```
arctl [GLOBAL OPTIONS] command [COMMAND OPTIONS] [ARGUMENTS...]
```

# GLOBAL OPTIONS

**--address**="": Armada maintenance API address. (default: "127.0.0.1:8445")

**--ca**="": Path to the client CA certificate.

**--help, -h**: show help

**--json**: Enables JSON logging.

**--token**="": The access token to use for authentication.


# COMMANDS

## backup

Backup Armada to local files.

**--dir**="": Target directory (current directory if empty).

## restore

Restore Armada from local files.

**--dir**="": Directory containing the backups (current directory if empty)

## version

Print current version.

## help, h

Shows a list of commands or help for one command
