# Running Armada

There are multiple possibilities for how to run Armada -- building Armada from source and executing the binary,
using the official Armada Docker image, or deploying Armada to Kubernetes via Helm Charts, which is the
recommended way.

> **Important:**
In order to run Armada, TLS certificates and keys must be provided. For testing purposes,
[certificate and key present in the repository](https://github.com/armadakv/armada/tree/cfc58f0205484b0c8a24c7cbcc0be8563b7cf6a5/hack)
can be used.

## Run binary

### Build from source

To build and run Armada locally, see the [Contribution page](contributing.md) for all
the required dependencies. Then just run

```bash
git clone git@github.com:armadakv/armada.git && cd armada
make run
```

This command will start an Armada leader cluster with a single instance locally.

### Download released binary

You can also download binary from the [Releases GitHub Page](https://github.com/armadakv/armada/releases).
After downloading the binary for the given platform, unzip the archive and run the following command:

```bash
tar -xf armada-darwin-amd64.tar
./armada leader \
    --dev-mode \
    --raft.address=127.0.0.1:5012 \
    --raft.initial-members='1=127.0.0.1:5012'
```
This command will start an Armada leader cluster with a single instance locally.

Create the `armada-test` table using the API.
```bash
grpcurl -plaintext -d "{\"name\": \"armada-test\"}" 127.0.0.1:8443 regatta.v1.Tables/Create
```

## Pull and run official Docker image

Official Armada images are present in
the GitHub Container Registry package at
[`armadakv/armada`](https://github.com/armadakv/armada/pkgs/container/regatta). The package path still uses the original `regatta` name.
Just execute `docker run` with the following arguments:

```bash
docker run \
    -p 8443:8443 \
    ghcr.io/armadakv/armada:latest \
    leader \
    --dev-mode \
    --raft.address=127.0.0.1:5012 \
    --raft.initial-members='1=127.0.0.1:5012'
```

This command will start an Armada leader cluster with a single instance in a Docker container.

Create the `armada-test` table using the API.
```bash
grpcurl -plaintext -d "{\"name\": \"armada-test\"}" 127.0.0.1:8443 regatta.v1.Tables/Create
```

## Deploy to Kubernetes from Helm Chart

To easily deploy Armada to Kubernetes, the official [Armada Helm Chart](https://github.com/armadakv/armada-helm) can be used.
The Helm repository and chart name still use the original `regatta` naming.

```bash
helm repo add regatta https://armadakv.github.io/armada-helm
helm repo update
helm install armada regatta/regatta
```

This will deploy Armada leader cluster with one replica. See page
[Deploying to Kubernetes](operations_guide/deploying_to_kubernetes.md) for the more advanced deployment
of Armada and how to connect follower clusters to the leader cluster.

## Interact with Armada

Once Armada is up and running, check the [User Guide page](user_guide/index.md) to see how
to interact with Armada.
