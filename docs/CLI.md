# CLI contract

Stable commands. Range may add flags, but existing flags keep their meaning.

Flags may appear before or after positional arguments. Everything after a bare
`--` is the workload command, so it keeps its own flags.

## The environment surface

These commands cover normal use. None of them requires you to know that a block
device is involved.

```bash
range build --from-oci IMAGE [-o FILE]    # an environment from a container image
range build <dir> [-o FILE]               # one from a directory
range publish <artifact> s3://bucket/key  # upload it, once
range shell <uri>                         # enter it, without downloading it
range shell python:3.12                   # or any container image, from its registry
range shell <uri> --mount hf://org/model:/model   # with a model repository inside
range run <uri> [-- COMMAND...]           # run one command inside it
range mount <uri> <dir>                   # mount it read-only on this Linux host
range index IMAGE... [-o DIR]             # index images for a catalog, no root needed
range serve <uri> [--addr HOST:PORT]      # expose it as a local URL, by range
range inspect <uri>                       # identity, size, cache state
```

By default, `range build` writes a compressed [Range artifact](ARTIFACT_FORMAT.md)
named `<image>.range`. When Range reads a chunk, it verifies the chunk against
the SHA-256 in the index. A mismatch is an error, and Range does not serve the
chunk into a filesystem.

| `build` flag | Default | Meaning |
| --- | --- | --- |
| `--from-oci IMAGE` | none | Build from a container image. No container runtime is needed |
| `--fs erofs\|ext4` | `erofs` | Filesystem inside. ext4 is for kernels without EROFS |
| `--size SIZE` | `20GiB` | Logical size, ext4 only. EROFS is sized to its contents |
| `--output`, `-o` | `<image>.range` | Where to write it |
| `--format range\|raw` | `range` | Compressed artifact, or a plain image you can loop-mount |
| `--chunk-size SIZE` | `1MiB` | Chunk size. Range aligns large files to it |

`<uri>` is one of these:

| Form | What it is |
| --- | --- |
| `s3://bucket/key`, `https://host/path`, a local path | A Range artifact or a raw EROFS or ext4 image |
| `python:3.12`, `ghcr.io/org/app:tag`, `name@sha256:...` | A container image, read from its registry. A bare name must not be a file here |
| `docker://NAME`, `oci://NAME` | The same, spelled out |
| `hf://org/name[@revision]` | A Hugging Face model repository, pinned to one commit |
| `hf://datasets/org/name`, `hf://spaces/org/name` | A dataset or Space repository |

Range reads a container image lazily. The first run of an image reads each layer
once, checks it against its digest and indexes it: the offset of every file, and
the points to resume gzip or zstd decompression from. Range keeps the index and
the layer in `$RANGE_CACHE_DIR/oci`. Later reads are ranged requests to the
registry, or reads of the kept layer, checked against a SHA-256 for every
64 KiB. Range first looks for a layer's index in the catalog,
[range-index](https://github.com/andreygrehov/range-index), and uses one only
if it names the same layer digest and size. `RANGE_INDEX_URL` sets another
catalog, and `RANGE_INDEX_URL=off` turns it off. The catalog also holds a
startup profile for popular images, so their first run prefetches the blocks
that startup reads. `RANGE_REGISTRY_MIRROR=mirror.gcr.io` reads Docker Hub
images from a mirror first, and falls back to Docker Hub. Range does not
support private registries yet.

A Hugging Face repository becomes a read-only filesystem of its files. Range
lists the files through the Hub API and reads each one with ranged requests.
Range does not support private or gated repositories yet.

`range serve` puts the same bytes behind HTTP on localhost (default
`127.0.0.1:8003`), at `/<last path element>`. GET, HEAD, single and multi-range
requests and `If-None-Match` work. The ETag is the artifact identity. Reads go
through the cache. Range records them under the workload name `serve` (change it
with `--workload`) and saves the profile on Ctrl-C.

Range answers a request only when its `Host` is an IP address or `localhost`. As
a result, a web page cannot reach the endpoint by rebinding its own domain.
`--allow-host a,b` admits other names.

`range publish` is the only command that writes to object storage. There is no
`range pull`: the whole artifact moves exactly once, when you publish it.

| `publish` flag | Default | Meaning |
| --- | --- | --- |
| `--part-size SIZE` | `64MiB` | Multipart part size. Minimum 5MiB |
| `--parallel N` | `8` | Concurrent part uploads |
| `--endpoint URL` | `$RANGE_S3_ENDPOINT` | S3-compatible endpoint |

## Environments

```bash
range shell <uri> [flags] [-- COMMAND...]
range run <uri> [flags] -- COMMAND...
```

`run` is `shell` with a command. With no command, it runs the environment's own
command: a container image's entrypoint and cmd, as `docker run` does. If the
environment has no command of its own, `run` refuses to start, so a CI job or an
agent cannot open an interactive shell by accident. Range resolves a
bare command name against the environment's own `PATH`, not the host's. As a
result, `range run <uri> -- go test ./...` finds the toolchain that the image
ships.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--workload NAME` | `interactive` | Profile identity, so unrelated workloads do not collide |
| `--workdir PATH` | from image metadata | Directory to start in |
| `--shell PATH` | from image metadata | Shell to execute |
| `--upper-dir PATH` | none | Where writes land. Setting it implies that they persist |
| `--name NAME` | none | Named environment whose writes survive exit |
| `--keep` | off | Keep this session's writable layer |
| `--profile off\|record\|auto` | `auto` | Working-set profile handling |
| `--prefetch-limit SIZE` | `256MiB` | Ceiling on profile prefetch |
| `--mount URI:/path` | none | Show another source read-only at `/path`, e.g. `hf://org/model:/model`. Repeatable |

With no `COMMAND`, an interactive shell starts. With a `COMMAND`, the command
runs non-interactively, and the exit status is the workload's:

```bash
range shell s3://bucket/dev.range --workload go-test -- go test ./...
```

This requires root, the `nbd` module, and `mount`, `umount`, `unshare` from
util-linux. Exports are read-only. Writes land in a local overlay.

A container image's entrypoint directory joins `PATH` when it is not there
already, so `llama-cli` in `ghcr.io/ggml-org/llama.cpp:light-b11206` runs by name.
Each `--mount` source gets its own cache and profile. Range mounts it with
`ro,nodev,nosuid`, and refuses a target path that crosses a symlink in the
environment.

## Mount

```bash
range mount <uri> <dir> [--readahead SIZE] [--profile off|record|auto]
```

Linux only, as root. Range attaches the source as a block device and mounts it
read-only with `nodev,nosuid` on `<dir>` until Ctrl-C. Any process on the host
reads the files from there. `--readahead` (default `16MiB`) sets the device's
readahead, so programs that map files, like model loaders, read in large pieces.

The environment is an isolation boundary, not a security boundary. It has none
of these controls:

- a user namespace
- a network namespace
- a seccomp filter
- a capability drop
- a cgroup limit

The workload runs as root. `/dev` is a private tmpfs that holds only the
standard nodes and a private devpts. The host's disks are therefore not in front
of the workload. However, a determined process inside the environment is still
root on the host kernel. Do not use the environment as the only barrier between
you and code that you do not trust.

## Building

```bash
range build <rootfs-dir> [--output FILE]
range build --from-oci <image> [--output FILE]
```

`range build` writes an EROFS filesystem, in Go. It preserves permissions
(including setuid), ownership, hardlinks, symlinks, device nodes and mtimes. It
does not write extended attributes. It needs no `mkfs` and no e2fsprogs. The
image is exactly as large as its contents. The same tree always produces the
same bytes.

Range aligns files of a chunk or more to the chunk size, so identical files
produce identical chunks across artifacts. `--fs ext4 --size SIZE` builds the
previous way and needs `mkfs.ext4`.

`--from-oci` pulls a container image directly from its registry, with no
container runtime and no daemon. It carries the image's own `PATH`, environment
and working directory into the artifact. As a result, a shell on the artifact
behaves like the image:

```bash
range build --from-oci golang:1.23 --output go.range
```

`--platform linux/amd64` pulls the image for another architecture, so a Mac can
build for an x86 fleet. The default is the host's platform. Range records the
platform in the artifact. An `image@sha256:...` reference is pinned. Range checks
each of these against its digest: the manifest, the platform manifest that an
index names, and the config.

Range verifies each layer against its digest and applies it to a tree in
memory. Range never unpacks a layer onto the host, so a layer cannot write
outside the image through a symlink or hardlink. Ownership, mode and setuid bits
come from the layer headers. An EROFS build therefore needs no root and, on
macOS, no VM. `--fs ext4` unpacks to disk through `os.Root` and needs root to
keep ownership. If you run it without root, it refuses.

## Debug

The plumbing is still reachable, but it is not in the way.

```bash
range debug read <uri> --offset N --length N   # a byte range, to stdout
range debug cat <uri>                          # the whole artifact, to stdout
range debug nbd serve <uri> [--addr host:port] # export over TCP, any platform
range debug nbd attach <uri> /dev/nbdN         # attach to the kernel, Linux
range debug info <uri>                         # same as range inspect
```

Exports are read-only. `--read-only=false` is an error, not a silent downgrade.

## Profiles

```bash
range profile path   <uri> [--workload NAME]
range profile show   <uri> [--workload NAME]
range profile export <uri> [--workload NAME] [--output FILE]
range profile import <file>
range profile clear  [<uri>] [--workload NAME]
```

Exported profiles are portable. If you copy one to another machine and import
it, the next cold run starts with a learned working set. See `PROFILE_FORMAT.md`.

## State

```bash
range stats [<uri>]      # session and lifetime metrics
range cache stats        # local byte cache usage
range cache clear [uri]  # remove materialized bytes only
range profile clear      # remove learned profiles only
range reset [uri]        # remove both
```

The byte cache and the learned profiles are separate state. Benchmarks need to
clear them independently. For this reason, no single command silently clears
both, except `reset`.

## Host

```bash
range doctor             # what this host needs, where each piece comes from, and what it doesn't need
```

## Common flags

The artifact commands accept these flags:

| Flag | Environment variable | Default |
| --- | --- | --- |
| `--block-size` | `RANGE_BLOCK_SIZE` | `1MiB` |
| `--memory-cache` | `RANGE_MEMORY_CACHE_SIZE` | `64MiB` |
| `--cache-size` | `RANGE_DISK_CACHE_SIZE` | `10GiB` |
| `--cache-dir` | `RANGE_CACHE_DIR` | `~/.cache/range` |
| `--max-range` | `RANGE_MAX_RANGE_SIZE` | `8MiB` |
| `--prefetch on\|off` | `RANGE_PREFETCH` | `on` |
| `--profile` | `RANGE_PROFILE` | `auto` |
| `--prefetch-limit` | `RANGE_PREFETCH_LIMIT` | `256MiB` |
| `--trace PATH` | none | off |

Other environment variables:

| Variable | Purpose |
| --- | --- |
| `RANGE_S3_ENDPOINT` | S3-compatible endpoint (R2, MinIO). Setting it enables path-style URLs |
| `RANGE_RUNTIME` | `lima` runs environments in a Lima VM on a Mac that can boot Range's own VM |
| `RANGE_VM_ASSETS` | A local copy of the VM archive (`scripts/vm-assets.sh` builds it), checked like a download |
| `RANGE_LIMA_INSTANCE` | Lima VM to use on macOS (default `range-linux`) |

## macOS

`range shell` and `range run` work on macOS. On Apple silicon with macOS 14 or
later, range boots a Linux VM for each session with Virtualization.framework and
stops it when the session ends. The VM boots from a kernel and an initramfs,
with no disk image of its own. The environment is a disk that range serves from
the Mac over NBD. Range Core, your credentials, the byte cache and the profiles
all stay on the Mac.

The first run downloads the VM archive once (13 MB) and checks it against the
SHA-256 in the source. Range signs a copy of itself for Virtualization.framework
with an ad-hoc signature, once for each build. No developer account is needed.
A tag is resolved once, then from a local cache. Range checks the tag again in
the background, and the next session uses a tag that has moved.

On an Intel Mac, or with `RANGE_RUNTIME=lima`, range runs environments in a Lima
VM (`brew install lima`). It creates the VM on first use and reaches it over NBD
through an ssh tunnel. `range build` runs in the Lima VM.

Both need a Linux build of range. Range takes it from one of these sources:

- `RANGE_GUEST_BINARY`
- a `range-linux-<arch>` beside the binary
- a cross-compile, when you run range from the source tree

`range doctor` reports the state.

## Internal

`__child` and `__guest` are internal re-execution entry points. They are not a
stable interface.
