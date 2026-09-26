# `.rangeprofile` v1

A profile records what one **workload** demanded from one **artifact**. The next
session uses the profile to fetch those blocks before the workload demands them.

Profiles are ordinary JSON files. They contain nothing machine-specific. You can
copy them between machines, store them in S3, commit them to git, or share them
as CI build outputs.

Profiles never change what a read returns. Deleting a profile costs only speed.

## Location

```
<cache-dir>/profiles/<key>.json
```

`<key>` is `SHA256(uri \0 size \0 etag \0 versionId \0 blockSize \0 workload)`,
hex encoded. The key depends only on the artifact's immutable identity, the
block size and the workload name. It never depends on a hostname, a path on this
machine or a user id. `range profile path <uri> --workload NAME` prints the
profile path.

## Schema

```json
{
  "version": 1,
  "artifact": {
    "uri": "s3://range-demo/acme-dev.range",
    "size": 107374182400,
    "etag": "38ba...",
    "versionId": "optional"
  },
  "blockSize": 1048576,
  "workload": "go-test",
  "sessions": 7,
  "updatedAt": "2026-09-20T00:58:48-04:00",
  "ranges": [
    {
      "startBlock": 12,
      "count": 4,
      "observations": 6,
      "firstSeenMs": 30,
      "meanFirstUseMs": 40,
      "lastSeen": "2026-09-20T00:58:48-04:00"
    }
  ]
}
```

### Fields

| Field | Meaning |
| --- | --- |
| `version` | Format major version. A reader must reject any value that it does not know. |
| `artifact.uri` | Location of the artifact. Part of the identity. |
| `artifact.size` | Size in bytes at the time of recording. |
| `artifact.etag` | Entity tag observed at open. |
| `artifact.versionId` | Object version, if the store has versioning. Omitted otherwise. |
| `blockSize` | Block size of the recorded observations. Block numbers mean nothing without it. |
| `workload` | Name of the workload that ran. `interactive` when unspecified. |
| `sessions` | Number of sessions that contributed to this profile. |
| `updatedAt` | Time of the last merge into this profile. |
| `ranges[]` | Runs of consecutive blocks, in first-observed order. This document calls each entry a *run*. |

### Range fields

| Field | Meaning |
| --- | --- |
| `startBlock` | First block of the run. |
| `count` | Number of consecutive blocks. |
| `observations` | Number of sessions that demanded this run. |
| `firstSeenMs` | Earliest first use ever observed, in milliseconds from session start. |
| `meanFirstUseMs` | Observation-weighted mean first use, in milliseconds from session start. |
| `lastSeen` | RFC 3339 time of the most recent session that demanded the run. |

### Timing semantics

Both timing fields are **milliseconds from the start of the session**. They
measure the *first use* of a block: the moment when the workload first demands
the block, not the moment of the fetch. A block that the cache serves entirely
still records its first-use time.

Timings are quantised to 10 ms. Quantisation lets adjacent blocks that the
workload reads in one burst form a single run. A run forms only from blocks whose
observation count and quantised timings agree exactly.

A run stores the same quantised values that each block in it would store on its
own. As a result, expanding a profile and merging it again reproduces the stored
values. The stored values do not drift further each session.

## Identity rules

A reader uses a profile only if **all** of these match the artifact that it
opens:

- `version` is 1
- `artifact.uri`, `artifact.size`, `artifact.etag`, `artifact.versionId`
- `blockSize`
- `workload`

On any mismatch, the reader ignores the profile. It does not adapt the profile.
Block numbers recorded against one generation of an artifact point at unrelated
bytes in another generation.

## Merge behaviour

A session never overwrites a profile. When a session completes, Range does
these steps:

1. It takes a lock.
2. It reads the existing profile.
3. It merges the session into the profile, with the rules below.
4. It writes the result atomically.

Merge rules:

- the merge takes the **union** of the blocks
- `observations` increments for every run that the session demanded
- `firstSeenMs` keeps the **minimum** ever observed
- `meanFirstUseMs` becomes the observation-weighted mean
- `lastSeen` advances
- `sessions` increments once per session

## Atomicity and locking

Range writes to a temporary file in the same directory, then renames the file
into place. A crash can therefore never leave a partially written profile.

Concurrent sessions that update the same profile serialise on an exclusive
`flock(2)` on `<path>.lock`. The kernel releases the lock when its holder exits,
so a session that crashes never leaves the lock held. A session waits at most 30
seconds for the lock. After that, it reports an error and does not save its
observations.

## Compatibility promise

- A reader **must reject** a `version` it does not support.
- A v1 reader **must tolerate** unknown additive fields, at both the top level
  and inside a run. New optional fields may be added within v1.
- The meaning of an existing v1 field will not change silently. A change in
  meaning requires a new `version`.

## Prefetch ranking

The ranking is deliberately simple and deterministic. Range orders runs by
these keys:

1. `observations`, descending (blocks demanded in the most sessions first)
2. `meanFirstUseMs`, ascending (blocks demanded earliest first)
3. stored order, as a tie-break

Prefetch stops when it spends its budget (`--prefetch-limit`, default 256 MiB).
Prefetch always yields to demand reads.
