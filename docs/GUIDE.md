# Guide

This guide explains how to use Range, what Range needs, and where its limits are. For the command
reference, see [CLI.md](CLI.md). For measurements, see [BENCHMARKS.md](BENCHMARKS.md).

## Instant environments

`range shell` turns an artifact into a working environment. The chain is `artifact -> nbd -> read-only EROFS -> overlay -> namespaces -> shell`.
The base image stays immutable. Every write lands in a local overlay, so `echo hello > /tmp/test`
never touches S3. When you exit the shell, Range unwinds the mounts, the device and the session
directory in reverse order.

```bash
range shell <uri>
    --workdir PATH          directory to start in
    --shell PATH            shell to execute
    --profile off|record|auto
    --prefetch-limit SIZE   ceiling on profile prefetch (default 256MiB)
    --upper-dir PATH        keep writes in a directory of your choosing
    --name NAME             named environment whose writes survive exit
    --keep                  keep this session's writable layer
```

With no command, `range shell` starts an interactive shell. With a command, it runs the command
non-interactively and exits with the workload's status:

```bash
range shell s3://bucket/dev.range --workload go-test -- go test ./...
```

`range shell` requires:

- root
- the `nbd` kernel module, which range loads itself
- EROFS and overlay in the kernel
- `mount`, `umount` and `unshare` from util-linux

An image can carry `/etc/range/environment.json` to set its own name, shell, working directory,
hostname and environment variables. Without this file, the defaults are `/bin/sh` in `/` on host
`range`.

## Container images and model repositories

`range shell` also opens container images and Hugging Face repositories as they
are, with no `range build` and no pull:

```bash
range shell python:3.12
range run ghcr.io/ggml-org/llama.cpp:light-b11206 --mount hf://unsloth/gemma-3-270m-it-GGUF:/model -- \
  llama-cli -m /model/gemma-3-270m-it-Q4_K_M.gguf -st -p "Why is the sky blue?"
range run python:3.12 --mount hf://moonshotai/Kimi-K2-Instruct:/model -- du -sh --apparent-size /model
```

The first run of an image fetches each layer's index from the catalog,
[range-index](https://github.com/andreygrehov/range-index). For a layer not in the
catalog, Range reads the layer once, as `docker pull` does, to index it. The index lists every file with its offset in the layer, and the
points to resume decompression from. For `python:3.12` the indexes take 4.1 MB.
Range keeps them, and the layers it read, in `$RANGE_CACHE_DIR/oci`, so the next
run reads only what the command touches. `range cache clear` with no argument
removes them.

Range reads gzip layers from the middle through the index. A zstd layer made of
many frames, like zstd:chunked, reads frame by frame. A zstd layer that is one
frame decodes forward from its start, and Range keeps what it decoded, so it
decodes each byte once.

A Hugging Face repository becomes a read-only filesystem of its files, pinned to
the commit its revision named at open. `--mount` puts one inside an environment.
`du` and `ls` read only metadata, so they move nothing. A program that reads a
whole model file still downloads that file, once.

`--gpus all` shows this machine's NVIDIA GPUs inside the environment, on Linux
as root, with the same flag as `docker run --gpus all`:

```bash
sudo range run --gpus all ghcr.io/ggml-org/llama.cpp:light-cuda-b11206 \
  --mount hf://unsloth/gemma-3-270m-it-GGUF:/model -- \
  llama-cli -m /model/gemma-3-270m-it-Q4_K_M.gguf -ngl 99 -st -p "Why is the sky blue?"
```

Range keeps CUDA's compiled kernels between sessions. On an EC2 g4dn.xlarge
(Tesla T4), this command took 136.7 s the first time and 3.1 s after that. With
Docker and nvidia-container-toolkit, every run took 112 to 127 s, image and model
already on the machine: CUDA compiled the image's kernels for the T4 again each
time, because the container's cache went with the container.

Not supported yet: private registries (`docker login`), private or gated Hugging
Face repositories, the image's `USER` (the workload runs as root), and GPUs in
Range's VM, on a Mac or on Linux without root.

## EBS snapshots and AMIs

`range shell` opens an EBS snapshot, or the root disk of an AMI, from any machine
with AWS credentials. It needs no volume, no instance and no network path to
your VPC:

```bash
range shell ebs://ami-0123456789abcdef0
range run ebs://snap-0123456789abcdef0 -- cat /etc/os-release
```

From a MacBook over wifi, a command in an Ubuntu 24.04 snapshot finished in 6.3 s
and moved 34.6 MB of its 7.5 GB. Starting an instance from the AMI and running
the same command over ssh took 21.9 s. A volume made from the snapshot,
attached to an instance already running and mounted there, took 21.1 s.

An x86 server's snapshot opens on an Apple silicon Mac too. Its programs cannot
run there, so the shell and its commands are Range's own, from busybox, and the
files are the snapshot's. Range reads ext4 and XFS, the filesystems of Ubuntu,
Debian, Amazon Linux and RHEL. AWS serves only snapshots that your account owns
or that another account shared with it. Copy a public one first with
`aws ec2 copy-snapshot`.

## Platforms

| Host | How it runs | Status |
| --- | --- | --- |
| Linux, as root | Native: nbd, EROFS, overlay, namespaces on this kernel | Verified end to end |
| Linux, without root | A VM for each session, with QEMU and KVM: the kvm group and QEMU, no root. The environment is the VM's disk, served over NBD | Verified end to end on arm64 |
| macOS, Apple silicon | Range Core stays on the Mac. Range boots a Linux VM for each session with Virtualization.framework. The environment is the VM's disk, served from the Mac over NBD. Nothing to install | Verified end to end |
| macOS, Intel | Range Core stays on the Mac. A Lima VM runs the environment, reached over NBD through an ssh reverse tunnel. Range creates the VM and installs a Linux build of itself on first use | Verified end to end |
| Windows | Run the Linux build inside WSL2 | Documented, not executed |

Range Core (credentials, byte cache, profiles) always runs on the host. The VM is a disposable
execution substrate, so there is one cache, one profile store and one credential path everywhere.
Building an EROFS image needs no Linux and runs natively on either host. Only `--fs ext4` builds
go through the VM on macOS. `range doctor` reports the strategy for this host and anything missing.

Windows has no native binary: the namespace and nbd code has no Windows analogue.

## Building an image

From a container image, with no container runtime installed:

```bash
range build --from-oci golang:1.23 --output go.range
```

The output is a compressed Range artifact that holds an EROFS filesystem. EROFS is the kernel's
read-only filesystem, in mainline since 5.4. Nothing writes to the base of an environment, so a
filesystem with a journal and an allocator was the wrong tool. Range writes EROFS itself, in Go,
so there is no `mkfs` and no e2fsprogs. `--fs ext4` builds the old way for kernels without EROFS.
`--format raw` skips the packing and leaves a plain image that you can loop-mount.

Range verifies layers against their digests and never unpacks them onto the host's filesystem.
Instead, Range:

1. applies the layers to a tree in memory
2. writes file contents to a content-addressed scratch store
3. writes the EROFS image from that tree

This has three results:

- A layer cannot escape the image through a symlink or a hardlink.
- Names that differ only in case stay distinct on a case-insensitive Mac.
- Ownership, mode and setuid bits come from the layer headers, not from the user who ran the build.

That is why an EROFS build needs no root, and on macOS it needs no VM. An image built as an
unprivileged user on a Mac is byte-identical to one built on Linux. It also matches a root ext4
build of the same image in owner, group, mode and content for every path. `--fs ext4` still
unpacks to disk (through `os.Root`), so it needs root to keep ownership. Without root, the build
refuses to produce an image where `/etc/shadow` belongs to you.

Range pulls the image directly from its registry. It carries the image's own `PATH`, environment
and working directory into the artifact, so a shell on the artifact behaves like the image.

Because the build runs anywhere, you choose the platform. The build does not inherit it.
`--platform linux/amd64` builds an x86 artifact on an Apple-silicon Mac for the fleet it will run
on. The default is the host's platform. Range records the platform in the artifact's
`environment.json`. At run time, Range refuses a mismatch and names the `--platform` to rebuild
with. Range checks the manifest, the platform manifest an index points to, the image config and
every layer against their digests. So `image@sha256:...` pins exactly what Range builds.

From a directory:

```bash
range build ./rootfs --output dev.range
```

An EROFS image is exactly as large as its contents, so `--size` only matters for `--fs ext4`. The
layout is a pure function of the source tree: the same tree gives the same bytes. Metadata sits
together at the front, so a cold start resolves paths from a handful of chunks.

Files of one chunk or more start on a chunk boundary. So an identical file lands in identical
chunks in every artifact that contains it. The toolchain that a thousand similar environments
share has the same chunk hashes in all of them. Nothing deduplicates across artifacts yet, and
there is no shared chunk store. But the identity that deduplication would need is in place.

Alignment is a trade-off, and the threshold comes from measurement, not from a guess. The padding
after an aligned file is zeros, which cost nothing over the network. But the cache decompresses
and stores logical bytes, and a learned profile replays all of them. The table shows the benchmark
above, on the same host, as medians of 3. The quarter-chunk and no-alignment rows come from one
run, and the one-chunk row comes from a second run:

| Align files of at least | Logical working set | Cold, bytes | Cold | With a profile |
| --- | ---: | ---: | ---: | ---: |
| a quarter chunk | 544 MB | 80 MB | 7.39 s | 3.65 s |
| **one chunk (default)** | **396 MB** | **89 MB** | **7.08 s** | **2.78 s** |
| nothing | 395 MB | 107 MB | 7.44 s | 2.62 s |

Aligning from a quarter chunk saved 11% of the network and cost 30% on every profiled start. So
the default aligns only files that fill a chunk. A native build from `golang:1.23` on macOS, with
no VM, took 19.8 s for the whole pull and build.

The writer was checked against the kernel, not only against itself. The check used an EROFS image
of a stock Ubuntu `/usr` (2.3 GB, 113,785 entries), mounted with `mount -t erofs`. The image
matches the source in content (`diff -r`). It also matches in mode, owner, link count, size,
symlink target and mtime for every entry, with setuid bits and hardlinks intact. The writer does
not write extended attributes, so file capabilities are lost. The ext4 path already lost them.

## What it needs, and what it doesn't

Range is not faster than every lazy loader. [Measured](BENCHMARKS.md#against-the-other-lazy-loaders)
against eStargz, it is at parity. The difference is how many moving parts it takes:

| | Installed on the host | Also needed |
| --- | --- | --- |
| SOCI | containerd, soci-snapshotter daemon, FUSE, a containerd config change (and lazy loading only happens under CRI) | a registry, a SOCI index built and pushed separately |
| eStargz | containerd, stargz-snapshotter daemon, FUSE | a registry, image conversion |
| Nydus | containerd, nydus-snapshotter, nydusd daemon (FUSE or EROFS + fscache) | a registry, conversion with nydusify |
| overlaybd | containerd, overlaybd-snapshotter, overlaybd-tcmu, TCMU in the kernel | a registry, image conversion |
| **Range** | **one binary, with nbd, EROFS and overlay from the kernel, and util-linux** | **any object on S3 or HTTP** |

SOCI and eStargz were installed and run for the benchmark above. The Nydus and overlaybd rows
come from their own documentation, and these two were not run here.

The four alternatives share one architecture: a plugin to containerd, with a long-running daemon,
a registry and a conversion pipeline. Range has nothing long-lived and no registry. That is a
claim about moving parts, not about dependencies. Range does need root and the `nbd` module on
Linux, and on macOS a Linux VM, which it boots itself. `range doctor` lists exactly that:

```
Needs
  root             ok              you
  mount, unshare   ok              util-linux, in every distribution
  nbd              ok              the kernel
  erofs            ok              the kernel, 5.4 and later
  overlay          ok              the kernel

Does not need
  a daemon, a registry, a container runtime, a snapshotter, FUSE
```

The rule behind that list: Range adds only what the kernel and a stock distribution do not
already have. On macOS, the guest VM gets nothing installed except Range itself. The NBD client
is built into Range, not borrowed from `nbd-client`.

Who this is for: if a team already runs Kubernetes, containerd is there and a snapshotter is one
Helm chart away. On Fargate, SOCI is built in. Fewer moving parts matters to people outside the
container stack:

- VM fleets for CI and RL
- bare EC2
- air-gapped hosts
- data that was never a container image

The closest competitor on minimalism is not SOCI. It is `nbdkit curl url=…` with its cache
filter, `nbd-client` and `mount`, which reproduces the lazy block read in one line. Above that
primitive, Range adds the artifact format (compression, sparseness, content hashes), learned
working sets, and `range shell`.

## Serving over HTTP

`range serve` puts the same `ReadAt` behind plain HTTP. Programs that already read by range then
get Range's cache, request coalescing and learned working set without knowing that Range exists:

```bash
$ range serve https://github.com/andreygrehov/range/releases/download/demo/go1.23-arm64.range
Serving https://github.com/andreygrehov/range/releases/download/demo/go1.23-arm64.range (1.03 GB)
  http://127.0.0.1:8003/go1.23-arm64.range
```

Any object works the same way, for example `range serve s3://bucket/events.parquet`. DuckDB,
SQLite's HTTP VFS, GDAL's `/vsicurl/`, ffmpeg and curl can point at the URL that it prints. GET,
HEAD, single and multi-range requests, and `If-None-Match` all work. The ETag is the artifact's
identity, so a client cache becomes invalid exactly when the object changes. For a Range
artifact, the served bytes are the decompressed, hash-verified logical bytes.

The server binds to localhost and reads with your cloud credentials, so it also checks the `Host`
header. A web page that rebinds its own domain to 127.0.0.1 reaches the port but gets a 403. The
server answers requests for IP addresses and `localhost`. Other names need `--allow-host`.

Against the public demo artifact on GitHub, from a laptop, a 64-byte range read cost 0.12 s and
one remote request the first time (median of three). The second time, it cost 0.001 s from the
cache.

A Go library would be the third way to use the same primitive. `archive/zip.NewReader` and
Parquet readers take an `io.ReaderAt`. But a library needs the core outside `package main`, and
that split does not exist yet.

## Working-set profiles

Each session records which blocks the workload demanded and how far into the session the workload
first asked for them. The session then merges that record into a profile under
`~/.cache/range/profiles/`. Profiles accumulate: observation counts, timings and a session count
grow run by run. Writes are atomic and locked. The next session replays the working set in the
background while the shell starts. Demand always wins: prefetch yields whenever a read is
outstanding, and prefetch stops at its budget.

A profile is `artifact + block size + workload`, so `go-test` and `npm-ci` never clobber each
other. If you change the artifact version or the block size, Range ignores the profile and does
not replay it against the wrong offsets.

Profiles are ordinary portable files:

```bash
range profile export s3://bucket/dev.range --workload go-test --output profile.json
# ... copy to another machine ...
range profile import profile.json
```

Format and merge semantics: [docs/PROFILE_FORMAT.md](PROFILE_FORMAT.md).

## The abstraction

One interface, and nothing above it:

```go
ReadAt(offset, length) → bytes
```

Range does these things:

- translates reads into range requests against S3 or HTTP
- caches fixed-size blocks on local disk
- collapses concurrent readers of the same block onto a single request
- merges adjacent misses into one request
- prefetches ahead of a sequential reader

Everything else (ext4, a VM disk, a database file) is somebody else's abstraction.

Range knows nothing about what its bytes mean.

## Run locally

Point Range at anything with a byte range: `s3://bucket/key`, `https://host/path`, or a local file.

```bash
./bin/range inspect   s3://demo/dev.range
./bin/range shell     s3://demo/dev.range
./bin/range run       s3://demo/dev.range -- go test ./...
./bin/range stats
./bin/range cache stats
./bin/range cache clear s3://demo/dev.range
```

The plumbing lives under `range debug`: raw reads, the NBD export and the builder. Normal use does
not require you to know that NBD is involved.

S3 access uses the standard AWS credential chain (environment, profile, EC2 or ECS role, web
identity). Range stores no credentials of its own and keeps credentials out of cache metadata,
logs and traces. Range needs `s3:HeadObject` and `s3:GetObject`, plus `s3:GetObjectVersion` on
versioned buckets.

## Mounting a filesystem

On Linux, `range mount` does all of it in one step, and cleans up on Ctrl-C:

```bash
sudo range mount hf://openai-community/gpt2 /mnt/gpt2
```

The steps it takes are also available one by one. `nbd attach` connects the artifact to a kernel block device. It needs Linux with the `nbd` module
loaded, plus an artifact whose size is a multiple of 4096:

```bash
sudo modprobe nbd
sudo ./bin/range debug nbd attach s3://demo/dev.range /dev/nbd0
sudo mount -o ro /dev/nbd0 /mnt/range
```

`nbd serve` exports over TCP instead. It works on any platform, including macOS, for development
and for clients such as `nbd-client` and QEMU:

```bash
./bin/range debug nbd serve s3://demo/dev.range --addr 127.0.0.1:10809
nbd-client 127.0.0.1 10809 /dev/nbd0
```

Exports are **read-only**. Range refuses writes with `EPERM`. `--read-only=false` is an error, not
a silent downgrade.

## Configuration

Flags override environment variables.

| Variable | Flag | Default | Purpose |
| --- | --- | --- | --- |
| `RANGE_BLOCK_SIZE` | `--block-size` | `1MiB` | Granularity of remote reads, minimum 4KiB |
| `RANGE_MEMORY_CACHE_SIZE` | `--memory-cache` | `64MiB` | In-process LRU above the disk cache |
| `RANGE_DISK_CACHE_SIZE` | `--cache-size` | `10GiB` | Disk cache limit. Oldest blocks are evicted |
| `RANGE_CACHE_DIR` | `--cache-dir` | `~/.cache/range` | Where blocks and statistics live |
| `RANGE_MAX_RANGE_SIZE` | `--max-range` | `8MiB` | Largest coalesced remote request |
| `RANGE_REQUEST_TIMEOUT` | `--request-timeout` | `30s` | Ceiling on one remote request |
| `RANGE_PREFETCH` | `--prefetch` | `on` | Sequential prefetch. `off` disables it |
| `RANGE_PROFILE` | `--profile` | `auto` | Working-set profile handling: `off`, `record` or `auto` |
| `RANGE_PREFETCH_LIMIT` | `--prefetch-limit` | `256MiB` | Ceiling on bytes pulled from a profile |
| `RANGE_S3_ENDPOINT` | none | none | S3-compatible endpoint (R2, MinIO). Enables path-style URLs |
| `RANGE_RUNTIME` | none | none | Linux: `native` or `kvm`. Mac: `lima` for a Lima VM |
| `RANGE_VM_ASSETS` | none | none | A local copy of the VM archive, checked like a download |
| `RANGE_LIMA_INSTANCE` | none | `range-linux` | Lima VM to use on macOS |

## Immutability and the cache

An opened artifact is immutable. Its cache key is a hash of URI, size, ETag, version and block
size. So a changed object is a different artifact, and its blocks never mix with the old ones. On
versioned S3 buckets, every read pins the version observed at open.

A block on disk is either absent or complete. Downloads land in a temporary file and Range renames
them into place, so a partial block is never visible. Concurrent processes can share a cache
directory safely.

Range pins reads to the generation observed at open:

- versioned S3 by `VersionId`
- unversioned S3 and HTTP by `If-Match`

If the object changes mid-session, the store returns 412. Range then fails loudly and does not
splice two generations together.

`range stats` separates this session from the artifact's lifetime. Range writes the stats every
five seconds while a session runs, so you can watch them from another terminal.

| Metric | Definition |
| --- | --- |
| Working set | Distinct bytes the workload demanded, counted once each |
| Remote transferred | Bytes moved, including refetches and prefetch |
| Fetch amplification | Remote transferred ÷ working set |

The byte cache and learned profiles are separate state. Use `range cache clear` for the byte
cache, `range profile clear` for profiles, or `range reset` for both.

## What this MVP does not do

These are out of scope:

- writable remote artifacts
- distributed or shared caches
- FUSE
- mmap
- a SQLite VFS
- containerd integration
- Merkle verification
- a `Rangefile`
- a registry
- native macOS or Windows environments

Range does build from OCI images and publish artifacts to S3. The `.range` format supports
compression, zero-chunk omission, chunk deduplication and SHA-256 verification on fetch.
Sequential prefetch and learned working-set profiles are also implemented. Range validates cached
blocks by length only.

**Size limits.** A raw object can be any size that S3 or the HTTP server will serve. A compressed
Range artifact holds at most 2^22 chunks: 4 TiB at the default 1 MiB chunk, 256 TiB at the 64 MiB
maximum. Range fetches the artifact's index whole when it opens the artifact, before the first
read. The index is 48 bytes per chunk before compression, 192 MiB at the limit. For the
environments Range builds, the index is a few kilobytes. For a multi-terabyte artifact, it is a
real cost at open.

Also not built:

- a chunk store shared between artifacts (chunk identity across artifacts is in place, but the
  storage that would use it is not)
- a Go library (see [Serving over HTTP](#serving-over-http))
- extended attributes in the EROFS writer

**Natively, the environment is not a security boundary.** In Range's VM, on a Mac or on Linux
without root, the workload runs under a kernel of its own. It still reaches the network and any
directory you share. The native environment has:

- no user namespace
- no network namespace
- no seccomp filter
- no capability drop
- no cgroup limit

Range uses `chroot`, not `pivot_root`, and `range shell` runs as root. `/dev` is a private tmpfs
with only the standard nodes and a private devpts, so the host's disks are not exposed by default.
But a determined process inside the environment is still root on the host kernel. Use a VM or a
real sandbox for code you do not trust.
