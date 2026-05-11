---
title: "armada backup"
section: "operations_guide"
subsection: "cli"
order: 4
---
## armada backup

Backup Armada to local files.

### Synopsis

Command backs up Armada into a directory of choice. All tables present in the target server are backed up.
Backup consists of file per a table in a binary compressed form and a human-readable manifest file. Use restore command to load backup into the server.

```
armada backup [flags]
```

### Options

```
      --address string   Armada maintenance API address. (default "127.0.0.1:8445")
      --ca string        Path to the client CA certificate.
      --dir string       Target directory (current directory if empty).
  -h, --help             help for backup
      --json             Enables JSON logging.
      --token string     The access token to use for the authentication.
```

### SEE ALSO

* [armada](armada.md)	 - Armada is a read-optimized distributed key-value store.

