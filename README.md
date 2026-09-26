# Range

Use a remote environment before downloading it.

Range opens a shell inside an environment that lives in S3, on an HTTP server or
in a local file, without pulling it first. It reads only the blocks your
workload touches, caches them, and remembers which ones the next run will need.

## Try it

No account or credentials needed. Pick the artifact for your machine:

```bash
range shell https://github.com/andreygrehov/range/releases/download/demo/go1.23-arm64.range   # Apple silicon, arm64 Linux
range shell https://github.com/andreygrehov/range/releases/download/demo/go1.23-amd64.range   # x86-64
```

```
Range environment
  Logical size      1.03 GB
  Ready             0.41 s

root@range:/go# go version
go version go1.23.12 linux/arm64
```

The artifacts are assets of a GitHub release. On EC2 in us-east-1, the shell is
ready in 0.41 s. From a laptop on home wifi, it is ready in 1.76 s. Of the
1.03 GB image, it read 24 MB and moved 6 MB over the network.

## Install

A release archive, for macOS or Linux on x86-64 or arm64:

```bash
curl -fsSL https://github.com/andreygrehov/range/releases/latest/download/range_$(uname -s)_$(uname -m).tar.gz | tar -xz
./range doctor
```

On macOS, environments run in a small Linux VM managed by [Lima](https://lima-vm.io)
(`brew install lima`). Range creates the VM on first use and installs nothing in
it but a Linux build of itself, which ships in the archive as
`range-linux-<arch>`. Keep it next to `range`.

From source, with Go 1.25 or newer:

```bash
git clone https://github.com/andreygrehov/range && cd range
make install    # into ~/.local/bin
```

## Make your own

```bash
range build --from-oci golang:1.23 -o go.range     # no container runtime needed
range publish go.range s3://<your-bucket>/go.range
range shell s3://<your-bucket>/go.range
range run s3://<your-bucket>/go.range -- go test ./...
```

`range build` also takes a directory. `--platform linux/amd64` builds for another
architecture, so a Mac can build for an x86 fleet.

## How it works

```
S3 / HTTP / file  ->  block cache  ->  /dev/nbdN  ->  EROFS  ->  overlay  ->  your shell
                          ^
                  learned working set
```

An artifact is one object: an EROFS filesystem in 1 MiB chunks, each compressed
on its own and checked against its SHA-256 when read. Range does not store chunks
of zeroes and stores identical chunks once. Range serves it to the kernel as a
network block device, mounts it read-only under a writable overlay, and starts
your command in fresh namespaces. Writes stay local, and Range discards them when
the session ends, unless you ask to keep them.

Each session records the blocks it needed. The next session fetches them in
the background before they are asked for, without ever delaying a real read.

## Numbers

The workload is `go build ./...` in a Go repository inside a 2.1 GB dev image.
Every arm ran on the same fresh EC2 host with nothing cached. Each value is the
median of five runs.

| Approach | Time | Network |
| --- | ---: | ---: |
| Pull the image, then run | 21.46 s | 652 MB |
| SOCI | 9.23 s | 202 MB |
| eStargz | 10.71 s | 102 MB |
| eStargz with prioritized files | 7.59 s | 101 MB |
| Range, first run | 7.33 s | 89 MB |
| Range, learned | 2.88 s | 90 MB |

Shipping a one-dependency change to five fresh workers took 1172 s by re-baking
an AMI and 100 s by rebuilding the artifact. Setup, method and the rest of the
measurements are in [docs/BENCHMARKS.md](docs/BENCHMARKS.md).

## Requirements

- Linux: root, the `nbd`, `erofs` and `overlay` kernel modules, and `mount` and
  `unshare` from util-linux.
- macOS: Lima.
- Windows: the Linux build inside WSL2, untested.

`range doctor` checks all of it and says what is missing.

## Limitations

- An environment is not a security boundary. It runs as root in namespaces on
  the host's network. Run untrusted code inside a VM.
- One architecture per artifact, as with container images.
- Artifacts are immutable. A changed object is a new artifact, and a session
  that sees its object change fails instead of mixing the two.
- Chunks are deduplicated within an artifact, not across artifacts yet.

## Documentation

- [Guide](docs/GUIDE.md): building images, serving over HTTP, profiles, configuration
- [CLI reference](docs/CLI.md)
- [Architecture](docs/ARCHITECTURE.md)
- [Artifact format](docs/ARTIFACT_FORMAT.md) and [profile format](docs/PROFILE_FORMAT.md)
- [Benchmarks](docs/BENCHMARKS.md)
- [Development](docs/DEVELOPMENT.md): building, testing, code layout

## License

Apache 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
