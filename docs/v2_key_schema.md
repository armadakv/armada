---
title: "V2 Key Schema"
description: "Details about Armada's V2 Key Schema for MVCC and high-performance range scans."
section: "overview"
order: 5
---

# V2 Key Schema

Armada's storage engine leverages a custom key schema (V2) to support Multi-Version Concurrency Control (MVCC) and to optimize data locality and range scan efficiency. This document outlines the design and reasoning behind the V2 Key Schema.

## Overview

In previous versions of the storage engine (V1), only the latest version of a key was kept in the state machine. Every update physically overwrote the prior value. This made features like Multi-Version Concurrency Control (MVCC) and incremental snapshots fundamentally impossible because the historical states were immediately lost.

The **V2 Key Schema** completely redesigns how keys are encoded physically inside the Pebble LSM-tree. The primary goal of this schema is to persist the **user key** alongside its **MVCC sequence number (seqno)**. By retaining historical versions of the data, Armada now unlocks full MVCC capabilities, point-in-time reads, and efficient incremental snapshotting without sacrificing point lookup or range scan performance.

The V2 physical key layout looks like this:

`[ Header 4B ] [ KeyType 1B ] [ UserKey ] [ 0x00 Separator 1B ] [ Seqno 8B ]`

### Structure Breakdown

1. **Header (4 bytes):** The first byte indicates the schema version (`0x02` for V2). The remaining 3 bytes are reserved.
2. **KeyType (1 byte):** Denotes whether this is a `User` key or a `System` key.
3. **UserKey (variable length):** The actual key string provided by the user.
4. **Separator (1 byte):** A single null byte (`0x00`) that separates the user key from the sequence number. This ensures correct lexicographical ordering between keys where one user key is a prefix of another.
5. **Seqno (8 bytes):** The MVCC sequence number (or Raft index).

## MVCC and Seqno Encoding

Armada is an MVCC-capable database. Every write operation gets a monotonically increasing sequence number (seqno), allowing the database to keep multiple versions of the same key.

To ensure that the *latest* version of a key sorts first during Pebble iterators, the 8-byte `Seqno` is encoded as **big-endian with every bit inverted**. 

For example:
* A `seqno` of `0` becomes `[FF FF FF FF FF FF FF FF]`.
* A `seqno` of `1` becomes `[FF FF FF FF FF FF FF FE]`.

By sorting the inverted sequence numbers lexicographically, Pebble natively surfaces the most recent write for a given `UserKey` before any of its older revisions.

## Pebble Integration

The V2 schema deeply integrates with Pebble's internal structures to optimize reads:

* **Prefix Extraction:** Armada configures Pebble's `Split` function to separate the prefix (everything up to and including the null separator) from the suffix (the `Seqno`). This allows Pebble's `SeekPrefixGE` to efficiently jump straight to the latest version of a specific `UserKey` while skipping older versions.
* **Block Properties:** Armada implements a custom Pebble block property collector (`armada.mvcc.seqno`). It tracks the `[min, max)` interval of MVCC seqnos present in each SST block and table. This enables powerful optimizations, such as fast garbage collection (GC) and efficient filtering of SSTables that do not contain relevant sequence numbers.

## Benefits

* **Faster Reads:** The inverted seqno and prefix extraction allow immediate access to the latest version of a key without scanning through old revisions.
* **Efficient Range Scans:** Range scans can efficiently skip unneeded data, and the null byte separator prevents prefix collision edge cases.
* **Smart Garbage Collection:** Block-level properties tracking seqno intervals allow the compaction and GC routines to drop entire SSTables containing expired MVCC data without iterating through the keys inside them.
