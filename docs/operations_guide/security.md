# Security

Armada exposes three gRPC endpoints — the user-facing **API**, the cross-cluster **Replication API**,
and the **Maintenance API** — plus a **REST** endpoint for metrics and health. Each can be secured
independently using TLS, mutual TLS (mTLS), and optional token-based authentication.

---

## Security Model

Each Armada endpoint has different trust requirements:

| Endpoint | Default port | Who connects | Recommended security |
|----------|-------------|--------------|----------------------|
| API (KV, Tables, Cluster) | `8443` | Application clients | TLS, optionally mTLS |
| Replication API | `8444` | Follower clusters only | mTLS |
| Maintenance API | `8445` | Operators / CI | TLS + token |
| REST (metrics/health) | `8080` | Prometheus, load balancers | Network-level isolation |

**Replication API** is the highest-risk endpoint because it streams the full contents of the
key-value store to any caller. Always protect it with mTLS in production so that only
authorized follower clusters can connect.

**Maintenance API** can trigger backups and restores. Always set `--maintenance.token` to a
strong random value in production.

**REST endpoint** does not support TLS. Restrict access using firewall rules or a reverse proxy.

---

## TLS Certificates

Every Armada gRPC endpoint requires a certificate and a private key to enable TLS.

### Generating Certificates for Development

For local development, self-signed certificates are available in the repository under
[`hack/`](https://github.com/armadakv/armada/tree/main/hack).

> **Warning:**
> Never use the repository certificates in a production environment — they are publicly known and
> should only be used for local testing.

### Generating Certificates for Production

Use a trusted CA (internal PKI, Let's Encrypt, or a cloud provider CA) to issue certificates.
The following example uses `openssl` to generate a simple self-signed CA and server/client
certificate pair for testing:

```bash
# 1. Generate CA key and certificate
openssl genrsa -out ca.key 4096
openssl req -new -x509 -days 3650 -key ca.key -out ca.crt \
    -subj "/CN=Armada CA"

# 2. Generate server key and CSR
openssl genrsa -out server.key 4096
openssl req -new -key server.key -out server.csr \
    -subj "/CN=armada-leader.example.com"

# 3. Sign the server certificate with the CA
openssl x509 -req -days 365 -in server.csr -CA ca.crt -CAkey ca.key \
    -CAcreateserial -out server.crt

# 4. Generate client key and certificate (for mTLS)
openssl genrsa -out client.key 4096
openssl req -new -key client.key -out client.csr \
    -subj "/CN=armada-follower"
openssl x509 -req -days 365 -in client.csr -CA ca.crt -CAkey ca.key \
    -CAcreateserial -out client.crt
```

### Certificate Rotation

Armada does not yet support automatic hot certificate rotation. To rotate certificates:

1. Replace the certificate and key files on disk.
2. Perform a rolling restart of the Armada cluster (one node at a time to maintain quorum).

---

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

This is useful for local development or when TLS is terminated by a sidecar / load balancer
in front of Armada.

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

---

## Replication API TLS (Leader)

The leader cluster's replication endpoint is configured with:

| Flag | Description |
|------|-------------|
| `--replication.cert-filename` | Path to the TLS certificate |
| `--replication.key-filename` | Path to the TLS private key |
| `--replication.ca-filename` | Path to the CA certificate (for client certificate auth) |
| `--replication.client-cert-auth` | Enable mTLS on the replication listener |

Because the Replication API streams the full dataset, always enable mTLS in production:

```bash
armada leader \
  --replication.cert-filename=server.crt \
  --replication.key-filename=server.key \
  --replication.ca-filename=ca.crt \
  --replication.client-cert-auth \
  ...
```

---

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

---

## Maintenance API Token

The Maintenance API (backup/restore) is protected by a token rather than a separate TLS
configuration. Set `--maintenance.token` on startup and pass the same value to the
`armada backup` or `armada restore` command via `--token`:

```bash
# Server
armada leader --maintenance.token=supersecret ...

# Client
armada backup --address=127.0.0.1:8445 --token=supersecret --ca=ca.crt --dir=/backup
```

If `--maintenance.token` is left empty (the default), no token is checked. Always set a
strong token in production.

> **Tip:** Use a randomly generated token of at least 32 characters, for example:
> ```bash
> openssl rand -hex 32
> ```

---

## Tables API Token

The Tables API (create/delete/list tables) can be protected by a token in the same way:

```bash
armada leader --tables.token=mytablestoken ...
```

---

## Unix Socket Transport

Armada can listen on a Unix domain socket instead of a TCP port. Use `unix` (plain) or `unixs`
(TLS) as the address scheme:

```bash
armada leader --api.address=unix:///run/armada/api.sock ...
```

Unix sockets are useful when the client and server run on the same host and you want to avoid
network overhead or open TCP ports.

---

## Address Scheme Summary

| Scheme | Transport | TLS |
|--------|-----------|-----|
| `http` | TCP | No |
| `https` | TCP | Yes |
| `unix` | Unix socket | No |
| `unixs` | Unix socket | Yes |

---

## Production Security Checklist

Use this checklist when hardening an Armada deployment:

- [ ] All gRPC endpoints use TLS (`https` or `unixs` scheme).
- [ ] The Replication API has mTLS enabled (`--replication.client-cert-auth`).
- [ ] Repository development certificates (`hack/`) are **not** used.
- [ ] `--maintenance.token` is set to a strong random value.
- [ ] `--tables.token` is set if table management access should be restricted.
- [ ] The REST metrics/health endpoint (`8080`) is not exposed to the public internet.
- [ ] Certificates are issued by an internal CA or a trusted public CA (not self-signed in production).
- [ ] Certificate expiry is monitored and a rotation procedure is documented.
- [ ] The replication port (`8444`) is firewalled to only accept connections from known follower cluster IPs.
- [ ] `--replication.insecure-skip-verify` is **not** set to `true` on followers.
