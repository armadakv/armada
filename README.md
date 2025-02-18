# Armada

[![tag](https://img.shields.io/github/tag/armadakv/armada.svg)](https://github.com/armadakv/armada/releases)
![GitHub go.mod Go version](https://img.shields.io/github/go-mod/go-version/armadakv/armada)
![Build Status](https://github.com/armadakv/armada/actions/workflows/test.yml/badge.svg)
[![Coverage Status](https://coveralls.io/repos/github/armadakv/armada/badge.svg)](https://coveralls.io/github/armadakv/armada)
[![Go report](https://goreportcard.com/badge/github.com/armadakv/armada)](https://goreportcard.com/report/github.com/armadakv/armada)
[![Contributors](https://img.shields.io/github/contributors/armadakv/armada)](https://github.com/armadakv/armada/graphs/contributors)
[![License](https://img.shields.io/github/license/armadakv/armada)](LICENSE)

**Armada** is a distributed ETCD inspired key-value store. Armada is designed to operate eiter as a standalone node,
standalone cluster or in Leader - Follower mode suited for distributing data in distant locations. e.g. in different
cloud regions.
While Armada maintains many of ETCD features there are some notable differences:

* Armada is designed to store much larger (tens of GB) datasets and also provide iterator-like API to query large
  datasets.
* Armada prioritize speed and performance over some more advanced ETCD features like Watch API, or Leases.
* Armada support multiple separate keyspaces called tables which operate individually.

## Production readiness

* Even though Armada has not yet reached the 1.0 milestone it is ready for a production use.
* There might be backward incompatible changes introduced until version 1.0, those will always be flagged in the release
  notes.
* Builds for tagged versions are provided in form of binaries in GH release, and Docker images.
* Tagged releases are suggested for production use, mainline builds should be used only for testing purposes.

## Why you should consider using Armada?

* You need to distribute data from a single cluster to multiple follower clusters in edge locations.
* You need a local, persistent, cache within a data center and reads heavily outnumber writes.
* You need a pseudo-document store.

## Documentation

For guidance on installation, deployment, and administration,
see the [documentation page](https://armadakv.io).

## Contributing

Armada is in active development and contributors are welcome! For guidance on development, see the page
[Contributing](...).
Feel free to ask questions and engage in [GitHub Discussions](https://github.com/armadakv/armada/discussions)!

