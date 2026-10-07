# Architecture

Two invariants govern everything here.

> **Everything is a file or a byte range. The server is always optional.**

Artifacts are ordinary objects in ordinary storage. Profiles are ordinary JSON
files. Nothing in the read path calls a Range service, because no Range service
exists. A hosted control plane could later sync profiles or share a cache.
Nothing below would change.

> **Range Core knows nothing about artifact semantics.**

Core sees offsets and lengths. It does not know what EROFS is, what a VM disk is,
or what a database file is. Everything that understands those things sits above
`ReadAt` and is somebody else's abstraction.

```
Size() -> N
ReadAt(offset, length) -> bytes
```

## Layers

```
  S3 / HTTP / file / image / Hugging Face   backend
                  |
                  v
         identity + block math            ReaderAt
     dedup, coalescing, retry, cache
                  |
        +---------+---------+
        |                   |
    disk cache          profile           working set, prefetch
    memory cache
                  |
                  v
        NBD server       HTTP server      interoperability surfaces
                  |      (range serve)
                  v
          /dev/nbdN  ->  EROFS            environment runtime
                  |
             OverlayFS (ro lower + rw upper)
                  |
              namespaces
                  |
               workload
```

| Layer | Package | Responsibility | Knows about |
| --- | --- | --- | --- |
| backend | `internal/object`, `internal/artifact` | Fetch one byte range. Report identity. Present an artifact's logical bytes | HTTP, S3, files, the artifact format |
| virtual image | `internal/virtual`, `internal/oci`, `internal/hub`, `internal/gzindex`, `internal/zstdindex` | Lay out an image or a Hub repository as EROFS in memory. Read each file from its layer or Hub file | registries, tar, gzip, zstd, the Hub API |
| ReaderAt | `internal/core`, `internal/cache` | Blocks, caching, dedup, coalescing, retry, immutability | offsets |
| profile | `internal/profile` | Which blocks a workload needs, and when | block numbers |
| NBD | `internal/nbd` | Expose `ReadAt` as a block device. On macOS, also the client side, in Go | the NBD protocol |
| HTTP | `cmd/range` (`range serve`) | Expose `ReadAt` as a URL that answers range requests | HTTP |
| builder | `internal/erofs`, `internal/oci`, `internal/image` | Write an EROFS image from a directory or OCI image, chunk-aligned | EROFS on-disk layout |
| environment | `internal/session` | EROFS or ext4, overlay, namespaces, the workload | Linux |
| CLI | `cmd/range` | Commands, flags, output | people |

Only the environment layer is Linux-specific. Everything above `ReadAt` and
below the environment is portable.

A container image or a Hugging Face repository is a backend too. Range lays the
source out as EROFS in memory: superblock, inodes and directories, a few
kilobytes to a few megabytes. Each file's data region maps to its source: an
offset in a layer's uncompressed tar stream, or a file on the Hub. A read of the
image becomes a read of the sources it covers. Core above it sees one object with
a size and an identity, as for any artifact.

Range keeps the layout of a container image on disk, named by its manifest digest.
A later session loads it and reads no layer index until it reads that layer.

An EBS snapshot is a backend as well. A read of its disk becomes requests for its
blocks of 512 KiB through the EBS direct APIs. Range shows the largest Linux
partition, and reads a block that the snapshot never wrote as zeros.

## Immutability

An opened artifact is one immutable generation. Its cache key is a hash of URI,
size, ETag, version and block size. A changed object is therefore a different
artifact, and the blocks of the two artifacts never mix.

Range pins reads to the generation that it observed at open:

- versioned S3: every `GetObject` carries the `VersionId`
- unversioned S3: every `GetObject` carries `If-Match: <etag>`
- HTTP: every ranged `GET` carries `If-Match: <etag>`
- container image: the identity is the platform manifest's digest, and Range
  checks every layer read against the layer's chunk hashes
- Hugging Face: the identity is the commit, and every URL names it

If the object changes underneath a session, the store returns `412`. Range then
fails loudly with `errObjectChanged`. It does not splice two generations of bytes
into one filesystem.

## Where work happens

Range Core always runs on the host, with the credentials, the byte cache and the
profiles. Only the kernel half needs Linux.

```
Linux                          macOS
-----                          -----
range (host)                   range (host)
  Range Core                     Range Core, credentials, cache, profiles
  nbd attach  ------+              |
  EROFS             |              | NBD, as the VM's disk (Virtualization.framework)
  overlay           |              v
  namespaces        |            Linux VM, one per session
  workload  <-------+              /dev/vda -> EROFS -> overlay -> namespaces
                                   workload
```

There is one cache implementation, one profile store and one credential path on
every platform. The VM is a disposable execution substrate, not a place where
state lives.

On Apple silicon, range boots the VM itself for each session, with
Virtualization.framework, from a kernel and an initramfs. Apple's NBD client
attaches the environment as `/dev/vda`, so the guest needs no NBD code at all.
The guest talks to the host over vsock: a control stream (the session, readiness,
the window size, the exit status) and one stream for each standard stream. On
an Intel Mac, range uses a Lima VM instead and installs nothing in it except range. The guest negotiates NBD itself: it runs the
client half of the fixed-newstyle handshake with `NBD_OPT_GO`. It then passes
the socket to the kernel with the same `NBD_SET_SOCK` that the native path uses.
The guest therefore needs no `nbd-client`.

There is no reconnect logic. The NBD connection uses the same ssh session as the
guest process. The connection therefore cannot drop while the session that it
serves is still alive.

## Dependencies

Range adds only what the kernel and a stock distribution do not already have.
The build writes EROFS in Go, so there is no `mkfs` or e2fsprogs. The guest
speaks NBD in Go, so there is no `nbd-client`. What remains is exactly the list
that `range doctor` prints:

- root
- a kernel with the `nbd`, `erofs` and `overlay` modules. Range loads `nbd`, and
  the kernel loads the other two when Range mounts those filesystems
- `mount`/`unshare` from util-linux

On a Mac with Apple silicon, the list is macOS 14 or later. Range downloads the
VM's kernel and initramfs itself. The Mac build uses cgo for
Virtualization.framework.

Windows runs the Linux build inside WSL2. A native Windows binary is not built,
because the namespace and nbd code has no Windows analogue.

## Read classes

Every fetch is either `demand` (a workload is blocked on it) or `prefetch`
(speculative). Demand always wins. Profile prefetch yields whenever a demand
fetch is in flight, and it never delays startup.

## Metrics

| Metric | Definition |
| --- | --- |
| Working set | Distinct bytes the workload demanded, counted once each |
| Working-set ratio | Working set / artifact size |
| Remote transferred | Bytes moved over the network, including refetches and prefetch |
| Transfer ratio | Remote transferred / artifact size |
| Fetch amplification | Remote transferred / working set |

Range records demand on the **logical** read path, before it consults the cache.
A warm run therefore reports the same working set as a cold one. Refetching a block
increases transfer but never the working set. Amplification below 1 means the
cache served most of the workload.
