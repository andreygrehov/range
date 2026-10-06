# Range

Use a remote environment before downloading it.

Range opens a shell inside a container image, a Hugging Face repository, or an
environment in S3, on an HTTP server or in a local file, without downloading it
first. It reads only the bytes your program touches, caches them, and remembers
which ones the next run will need. No Docker, no daemon and no pull.

## Try it

Any image from a container registry:

```bash
range shell python:3.12
```

With the current directory inside, read-write, the way `docker run -v` does it:

```bash
range run python:3.12 --mount .:/work --workdir /work -- python3 app.py
```

With a port published and a variable set, as `docker run -p` and `-e` do:

```bash
range run python:3.12 -p 8000 -e DEBUG=1 -- python3 -m http.server 8000
```

A chat model, from nothing, in one line. Range opens the llama.cpp image from its
registry and mounts the model repository at `/model`:

```
$ range run ghcr.io/ggml-org/llama.cpp:light-b11206 \
    --mount hf://unsloth/gemma-3-270m-it-GGUF:/model -- \
    llama-cli -m /model/gemma-3-270m-it-Q4_K_M.gguf -st \
    -p "Why is the sky blue? Answer in one sentence."

The sky is blue because of a phenomenon called Rayleigh scattering,
where blue light is scattered more than other colors.
```

A 1 TB model, open in seconds:

```
$ range run python:3.12 --mount hf://moonshotai/Kimi-K2-Instruct:/model -- \
    du -sh --apparent-size /model
959G    /model
```

Kimi K2 is 1.03 TB in 61 shards. A Python script inside read its config, the
header of one shard and one tensor in 3.4 s, and moved 9.5 MB of the model. The
other 60 shards never left Hugging Face. The chat answer took 6.7 s, where
`docker pull` plus `hf download` took 18.3 s. Both runs are on EC2 in us-east-1,
from an empty cache, with the image indexed once (see [Numbers](#numbers)).

## Install

A release archive, for macOS or Linux on x86-64 or arm64:

```bash
curl -fsSL https://github.com/andreygrehov/range/releases/latest/download/range_$(uname -s)_$(uname -m).tar.gz | tar -xz
./range doctor
```

On a Mac with Apple silicon and macOS 14 or later, Range boots a small Linux VM
for each session with Apple's Virtualization.framework. The VM starts in a
fraction of a second and stops with the session. There is nothing to install:
the first run downloads the VM's kernel once (13 MB). The VM runs a Linux build
of Range, which ships in the archive as `range-linux-<arch>`. Keep it next to
`range`. An Intel Mac runs environments in a [Lima](https://lima-vm.io) VM
(`brew install lima`).

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
image / Hugging Face / S3 / HTTP / file  ->  block cache  ->  /dev/nbdN  ->  EROFS  ->  overlay  ->  your shell
                                                  ^
                                          learned working set
```

A container image stays in its registry. The first run reads each layer once to
index it: where every file starts in the layer, and the points to resume gzip or
zstd decompression from. For `python:3.12` the index is 4.1 MB, 1% of the image.
After that, reading a
file costs one ranged request to the registry, checked against a SHA-256 for every
64 KiB. A Hugging Face repository works the same way, with the file list from the
Hub API pinned to one commit.

Popular images come pre-indexed from
[range-index](https://github.com/andreygrehov/range-index), so their first run is
lazy too.

An artifact is one object: an EROFS filesystem in 1 MiB chunks, each compressed
on its own and checked against its SHA-256 when read. Range does not store chunks
of zeroes and stores identical chunks once. Range serves it to the kernel as a
network block device, mounts it read-only under a writable overlay, and starts
your command in fresh namespaces. Writes stay local, and Range discards them when
the session ends, unless you ask to keep them.

Each session records the blocks it needed. The next session fetches them in
the background before they are asked for, without ever delaying a real read.

## Numbers

From an empty cache to the command's output, on an m6i.large in us-east-1,
28 September 2026. Medians of three. "Indexed" means Range indexed the image once
before. "Again" keeps the cache of the run before.

| Workload | docker pull | Range, first run | Range, indexed | Range, again |
| --- | ---: | ---: | ---: | ---: |
| Chat demo (llama.cpp + Gemma 270M) | 18.3 s, 579 MB | 15.5 s, 590 MB | 6.7 s, 317 MB | 4.3 s, 0 MB |
| `python:3.12`, import json and sqlite3 | 15.8 s, 435 MB | 16.5 s, 415 MB | 2.8 s, 48 MB | 1.2 s, 0 MB |
| `rust:1.82`, `cargo --version` | 19.6 s, 569 MB | 22.4 s, 546 MB | 8.0 s, 125 MB | 1.6 s, 0 MB |
| `eclipse-temurin:21`, `java -version` | 7.9 s, 232 MB | 9.2 s, 225 MB | 2.9 s, 49 MB | 1.0 s, 0 MB |
| Kimi K2 (1.03 TB), read one tensor | not tried | 17.6 s, 433 MB | 3.4 s, 56 MB | 1.3 s, 0 MB |

The first run of an image reads its layers whole, as `docker pull` does. Here it
took from 15% less to 16% more time than `docker pull`. The gain starts with the
second run of an image, or at once for an image in the catalog.

With a Range artifact, on 23 September 2026: the workload is `go build ./...` in a
Go repository inside a 2.1 GB dev image.
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

- Linux: root, a kernel with the `nbd`, `erofs` and `overlay` modules, which
  Range loads itself, and `mount` and `unshare` from util-linux.
- macOS: Apple silicon and macOS 14 or later. On an Intel Mac, Lima.
- Windows: the Linux build inside WSL2, untested.

`range doctor` checks all of it and says what is missing.

## Limitations

- The first run of an image outside the catalog reads its layers whole to
  index them.
  Private registries and gated Hugging Face models are not supported yet.
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
