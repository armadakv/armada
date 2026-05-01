# Deploying Armada to Kubernetes

In order to run Armada cluster, TLS certificates and keys must be generated for the gRPC APIs.

See
[Armada](https://github.com/armadakv/armada-helm/blob/3dc1954d2a08c4a983c7cef0c2e853bfa5ef65aa/charts/regatta/values.yaml#L114),
[Replication](https://github.com/armadakv/armada-helm/blob/3dc1954d2a08c4a983c7cef0c2e853bfa5ef65aa/charts/regatta/values.yaml#L197), and
[Maintenance API](https://github.com/armadakv/armada-helm/blob/3dc1954d2a08c4a983c7cef0c2e853bfa5ef65aa/charts/regatta/values.yaml#L285)
definitions in the Helm Chart or [`armada leader`](cli/armada_leader.md)
and [`armada follower`](cli/armada_follower.md) commands for reference. The Helm repository still uses the legacy `regatta` chart name.

## Deploying Armada leader cluster

To deploy Armada leader cluster, let's specify the following values:

```yaml
# values-leader.yaml

# Create Armada leader cluster with 3 instances.
mode: leader
replicas: 3

# Specify the tables.
tables: testing-table1,testing-table2

replication:
  # Armada leader cluster's replication gRPC API for the follower clusters to connect to.
  externalDomain: "leader.armada.example.com"
  port: 8444
```

Then run the following commands in the core cluster where the Armada leader cluster should reside:

```bash
helm repo add regatta https://armadakv.github.io/armada-helm
helm repo update
helm install armada-leader regatta/regatta -f values-leader.yaml
```

This will create an Armada leader cluster with 3 instances with Replication API for the follower clusters
exposed on `leader.armada.example.com:8444`.

## Connecting follower cluster

Armada follower cluster can be dynamically added to replicate data from the leader cluster without any
intervention done in the leader cluster. Just make sure the leader's Replication gRPC API
is reachable from the edge clusters where the Armada follower clusters will be deployed.

To deploy Armada follower cluster and connect it to the leader cluster, let's specify the following values:

```yaml
# values-follower.yaml
# Values for the Armada follower cluster.

# Create Armada follower cluster with 3 instances.
mode: follower
replicas: 3

replication:
  # Specify the address of the Armada leader cluster to asynchronously replicate data from.
  leaderAddress: "leader.armada.example.com:8444"

maintenance:
  server:
    # Optionally, disable maintenance API for the follower cluster.
    enabled: false
```

Then run the following commands in all the edge clusters where the Armada follower clusters should reside:

```bash
helm repo add regatta https://armadakv.github.io/armada-helm
helm repo update
helm install armada-follower regatta/regatta -f values-follower.yaml
```

This will create an Armada follower cluster with 3 instances asynchronously replicating data
from the Armada leader cluster `leader.armada.example.com:8444`. Once the Armada follower cluster is up
and running, it will immediately pull the data from the leader cluster without any manual intervention needed.

> **Important:**
Armada currently supports only static cluster members, therefore the number
of members cannot be changed once the cluster is running. To change the number of members in a cluster
when creating the cluster, see the
[Helm Chart](https://github.com/armadakv/armada-helm/blob/3dc1954d2a08c4a983c7cef0c2e853bfa5ef65aa/charts/regatta/values.yaml#L21).
