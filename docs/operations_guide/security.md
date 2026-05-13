# Security

Armada exposes three gRPC endpoints — the user-facing **API**, the cross-cluster **Replication API**,
and the **Maintenance API** — plus a **REST** endpoint for metrics and health. Each can be secured
independently using TLS, mutual TLS (mTLS), and optional token-based authentication.

## TLS Certificates

Every Armada gRPC endpoint requires a certificate and a private key to enable TLS.
For local development, self-signed certificates are available in the repository under
[`hack/`](https://github.com/armadakv/armada/tree/main/hack).

> **Warning:**
> Never use the repository certificates in a production environment — they are publicly known and
> should only be used for local testing.

## API Server TLS

The user-facing API is configured with the following flags:

| Flag | Description |
|------|-------------|
| `--api.cert-filename` | Path to the TLS certificate |
| `--api.key-filename` | Path to the TLS private key |
| `--api.ca-filename` | Path to the CA certificate (required for client certificate auth) |
| `--api.client-cert-auth` | Enable mTLS — clients must present a certificate signed by the CA |
| `--api.allowed-cn` | Restrict access to clients whose certificate CN matches this value |
| `--api.allowed-hostname` | Restrict access to clients whose certificate IP/hostname matches this value |

### Plain (non-TLS) API

The API server can be run without TLS by setting the address scheme to `http`:

```bash
armada leader --api.address=http://0.0.0.0:8443 ...
```

This is useful for local development or when TLS is terminated by a sidecar / load balancer in front of Armada.

### Mutual TLS (mTLS)

To require clients to present a certificate:

```bash
armada leader \
  --api.cert-filename=server.crt \
  --api.key-filename=server.key \
  --api.ca-filename=ca.crt \
  --api.client-cert-auth \
  ...
```

Optionally restrict to a specific CN or hostname:

```bash
  --api.allowed-cn=my-client
  # or
  --api.allowed-hostname=client.example.com
```

## Replication API TLS (Leader)

The leader cluster's replication endpoint is configured with:

| Flag | Description |
|------|-------------|
| `--replication.cert-filename` | Path to the TLS certificate |
| `--replication.key-filename` | Path to the TLS private key |
| `--replication.ca-filename` | Path to the CA certificate (for client certificate auth) |
| `--replication.client-cert-auth` | Enable mTLS on the replication listener |

## Replication Client TLS (Follower)

The follower cluster's replication client is configured with:

| Flag | Description |
|------|-------------|
| `--replication.cert-filename` | Path to the client TLS certificate |
| `--replication.key-filename` | Path to the client TLS private key |
| `--replication.ca-filename` | Path to the CA that signed the leader's certificate |
| `--replication.insecure-skip-verify` | Skip server certificate verification (not recommended for production) |
| `--replication.server-name` | Override the expected server hostname in the certificate |

### Example — follower connecting to leader with mTLS

```bash
armada follower \
  --replication.leader-address=leader.armada.example.com:8444 \
  --replication.cert-filename=hack/replication/client.crt \
  --replication.key-filename=hack/replication/client.key \
  --replication.ca-filename=hack/replication/ca.crt \
  ...
```

## Maintenance API Token

The Maintenance API (backup/restore) is protected by a token rather than a separate TLS configuration.
Set `--maintenance.token` on startup and pass the same value to the `armada backup` or `armada restore`
command via `--token`:

```bash
# Server
armada leader --maintenance.token=supersecret ...

# Client
armada backup --address=127.0.0.1:8445 --token=supersecret --ca=ca.crt --dir=/backup
```

If `--maintenance.token` is left empty (the default), no token is checked.

## Tables API Token

The Tables API (create/delete/list tables) can be protected by a token in the same way:

```bash
armada leader --tables.token=mytablestoken ...
```

## Unix Socket Transport

Armada can listen on a Unix domain socket instead of a TCP port. Use `unix` (plain) or `unixs` (TLS)
as the address scheme:

```bash
armada leader --api.address=unix:///run/armada/api.sock ...
```

Unix sockets are useful when the client and server run on the same host and you want to avoid
network overhead or open TCP ports.

## Address Scheme Summary

| Scheme | Transport | TLS |
|--------|-----------|-----|
| `http` | TCP | No |
| `https` | TCP | Yes |
| `unix` | Unix socket | No |
| `unixs` | Unix socket | Yes |
