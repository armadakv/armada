---
title: "Ecosystem"
description: "Libraries, SDKs, and community projects built around ArmadaKV."
section: "overview"
order: 5
---

# Ecosystem
Here you can find libraries, projects and SDKs related to the Armada project.

The `regatta-` prefixes in some tool and library names are a legacy of Armada's origins as a
fork of the Regatta project. See [Regatta and Armada](regatta.md) for the full story.

## CLIs
Armada ships its own official [`arctl`](operations_guide/cli/arctl.md) control-plane CLI for
maintenance and administration workflows such as backup, restore, and table management.
Community tools include:

* [regatta-client](https://github.com/Tantalor93/regatta-client) — unofficial CLI for querying and manipulating the Armada store.

## UI consoles
* [console](https://github.com/armadakv/console) — official web UI for querying, monitoring, and administration of an Armada cluster.

## Client libraries
* [armada-go](https://github.com/armadakv/armada-go) — official Go client library. *(Package still published under the legacy `regatta-go` module path.)*
* [armada-java-core](https://github.com/armadakv/armada-java/tree/main/regatta-java-core) — official low-level client library for JVM languages. *(Artifact still uses the legacy `regatta-java-core` name.)*
* [armada-java-spring-data](https://github.com/armadakv/armada-java/tree/main/regatta-java-spring-data) — official Spring Data support library for JVM languages. *(Artifact still uses the legacy `regatta-java-spring-data` name.)*
