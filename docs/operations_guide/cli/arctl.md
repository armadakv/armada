---
title: "arctl"
section: "operations_guide"
subsection: "cli"
order: 5
---
## arctl

Armada control CLI.

### Synopsis

Arctl provides administrative and maintenance commands for Armada clusters,
including backup and restore workflows.

### Options

```
      --address string   Armada maintenance API address. (default "127.0.0.1:8445")
      --ca string        Path to the client CA certificate.
  -h, --help             help for arctl
      --json             Enables JSON logging.
      --token string     The access token to use for the authentication.
```

### SEE ALSO

* [arctl backup](arctl_backup.md)	 - Backup Armada to local files.
* [arctl restore](arctl_restore.md)	 - Restore Armada from local files.

