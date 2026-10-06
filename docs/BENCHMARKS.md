# Benchmarks

Every number here is a measurement, with the setup next to it. Dates matter: the format
changed over time, and this page labels older runs as such.


## Real work on a Mac, against Docker Desktop

Measured 6 October 2026 on a MacBook Pro with Apple silicon, macOS 15.4, over wifi. Range 0.5
with its own VM (8 CPUs), against Docker Desktop 28.0.4 (16 CPUs). Each cell is the total time
of one command, from the prompt back to the prompt, median of three. All 48 runs succeeded.
Cold: nothing of the image on this Mac. Docker starts without the image, and Range starts from an
empty cache with its VM files kept. Warm: the same command again.

| Command | Image | Cold, Range | Cold, Docker | Warm, Range | Warm, Docker |
| --- | --- | ---: | ---: | ---: | ---: |
| `pip install requests numpy`, then import both | `python:3.12` | 12.2 s | 17.4 s | 4.5 s | 3.8 s |
| `python -m compileall` over the standard library | `python:3.12` | 10.3 s | 14.6 s | 2.2 s | 1.5 s |
| `cargo new` and `cargo build` | `rust:1.82` | 15.2 s | 16.7 s | 1.6 s | 0.6 s |
| `go build` of a program in a shared directory | `golang:1.23` | 10.9 s | 15.1 s | 4.2 s | 3.4 s |

Cold, Range is faster in every row. Before Range downloaded a heavily read layer whole, the
`cargo` row was 25.5 s against 18.1 s: a build reads most of the image, and ranged reads lost
to one bulk download. Warm, Docker Desktop is 0.6 to 1.0 s faster in every row. Its VM is
always running, with a warm page cache. Range boots a VM for each session, and every read in
it crosses NBD to the Mac. Giving Range's VM 16 CPUs did not change the warm numbers.

The `python:3.12` layers were only partly in the catalog that day, so Range indexed four of
the seven itself in the cold runs.

## Container images and model repositories, against docker pull

Measured 28 September 2026 on one EC2 m6i.large (2 vCPU, x86-64) in us-east-1, Ubuntu 24.04,
kernel 7.0, Docker 29.1 and `hf` 2.0 with hf-xet. Each cell is the time from an empty cache to
the command's output, and the bytes the host's network card received in that time. Medians of
three. All 57 runs printed the expected output.

| Workload | docker pull | Range, first run | Range, indexed | Range, again |
| --- | ---: | ---: | ---: | ---: |
| Chat demo: llama.cpp `light-b11206`, Gemma 3 270M Q4_K_M, one answer | 18.27 s, 579 MB | 15.46 s, 590 MB | 6.73 s, 317 MB | 4.25 s, 0 MB |
| `python:3.12`, `import sys, json, sqlite3` | 15.83 s, 435 MB | 16.48 s, 415 MB | 2.81 s, 48 MB | 1.15 s, 0 MB |
| `rust:1.82`, `cargo --version && rustc --version` | 19.64 s, 569 MB | 22.44 s, 546 MB | 7.99 s, 125 MB | 1.57 s, 0 MB |
| `eclipse-temurin:21`, `java -version` | 7.94 s, 232 MB | 9.20 s, 225 MB | 2.93 s, 49 MB | 1.01 s, 0 MB |
| `python:3.12` + Kimi K2 (1.03 TB), read one tensor | not tried | 17.63 s, 433 MB | 3.38 s, 56 MB | 1.25 s, 0 MB |

The columns:

- **docker pull.** `docker pull`, then `docker run`. For the chat demo, also
  `hf download` of the one model file before the run. Every run starts with the image removed
  and the Hugging Face cache empty.
- **Range, first run.** Nothing cached. Range reads every layer once to index it, as
  `docker pull` does, and keeps the layer.
- **Range, indexed.** The harness keeps the layer indexes and removes everything else: the
  kept layers, the block cache and the profiles. This is what a first run looks like when an
  index already exists.
- **Range, again.** The run after the first one, with its cache.

The chat demo pins `ghcr.io/ggml-org/llama.cpp:light-b11206`, the build these runs used. The
`light` tag moved to build 11223 later the same day, and that build fails to load its CPU backends
("no backends are loaded") under Range and outside it alike, so the demo does not follow the tag.

In the Kimi K2 row, Range's own counters show 8.41 MB read from the model and 9.46 MB fetched,
in every run. The other 48 MB are `python:3.12`. The Hugging Face files come straight from the
Hub. The chat answer in every run was "The sky is blue because of a phenomenon called Rayleigh
scattering, where blue light is scattered more than other colors."

The same day on a MacBook over residential wifi, through Range's Lima VM, with the indexes kept:
`python:3.12` was ready in 2.7 s and fetched 49 MB, `rust:1.82` 2.1 s and 132 MB,
`eclipse-temurin:21` 2.0 s and 76 MB. zstd:chunked images, read straight from the registry with
only the index kept: `ghcr.io/stargz-containers/ubuntu:24.04-zstdchunked` fetched 10.6 MB in
10 requests (the image is 31 MB), `python:3.13-slim-zstdchunked` 25.5 MB in 26 requests (51 MB).

## Time to shell

The target is the public demo: `golang:1.23`, a 278 MB artifact. The same bytes are
published twice, as a GitHub release asset and as an object in S3 (us-east-1).
Anyone can reproduce these rows. Time to shell is a handshake sent from inside the
environment immediately before the command runs. It is not an estimate.

Measured 27 September 2026, GitHub and S3 interleaved in each run:

| Client | State | GitHub | S3 |
| --- | --- | ---: | ---: |
| MacBook, residential wifi (arm64) | Cold | 1.76 s | 1.88 s |
| | Learned | 1.23 s | 1.36 s |
| | Warm | 1.15 s | 1.09 s |
| EC2 m7g.large, us-east-1 (arm64) | Cold | 0.41 s | 0.48 s |
| | Learned | 0.38 s | 0.40 s |
| | Warm | 0.33 s | 0.34 s |
| EC2 m6i.large, us-east-1 (x86-64) | Cold | 0.35 s | 0.47 s |
| | Learned | 0.36 s | 0.41 s |
| | Warm | 0.32 s | 0.33 s |

The laptop rows are medians of three, the EC2 rows medians of five. "Cold" means no cache
and no profile. "Learned" means the cache was cleared and the profile kept. "Warm" means
both were kept.

On arm64, a cold start reads 24.23 MB, which is 6.01 MB over the network in 24 requests.
A learned start reads 25.28 MB (6.02 MB over the network) in 16 requests. A warm start
reads nothing. The bytes are the same from both sources.

Every request to GitHub goes through a redirect to its storage. From EC2, GitHub was still
faster than S3 in this run. On the Mac, the second between the laptop and EC2 numbers is
the Lima VM and its ssh session, not the fetch: a warm start is within 0.1 s of a learned
one.

## In the cloud, and at fleet scale

The next run uses an m6i.2xlarge in the bucket's region and the 2.1 GB dev environment from the
benchmark below. There, the environment is ready in 0.93 s cold and 0.50 s with a learned
profile (median of five). Five fresh m7g.2xlarge workers launched at once, each with an empty
cache. They went from `run-instances` to a finished `go build` in 28.7 s. They were ready in
0.48–0.79 s and moved 452 MB between them
([When the environment changes](#when-the-environment-changes)).

An earlier run on 20 September used the raw ext4 format. It put a real AI coding agent on a
128.85 GB sparse environment (Ubuntu, Go, Node, Python, git, warm caches), on an m7g.2xlarge
in-region. It has not been re-run on EROFS:

| State | Environment ready | Time to running agent | Bytes moved |
| --- | ---: | ---: | ---: |
| Raw download-first | n/a | 997.96 s | 128.85 GB |
| Compressed (tar.zst) | n/a | 37.79 s | 1.84 GB |
| Range cold | 0.98 s | 2.82 s | 888 MB |
| Range + shared profile | 0.21 s | 1.85 s | 880 MB |

The raw arm is mostly downloading zeros of a sparse image, so tar.zst is the baseline that
matters there. Five fresh workers shared one artifact and one 127 KB profile. They reached a
running agent in 1.48 s mean, max 1.66 s, and moved 4.4 GB between them.

Range delivered the agent's environment. It did not contain the agent. The environment is not a
security boundary (see [what this MVP does not do](GUIDE.md#what-this-mvp-does-not-do)), so run
untrusted code inside a VM.

Two profiles trained independently on two machines converged on the identical 722-block working
set (Jaccard 1.0). The working set is a property of the artifact and workload, not the machine.

## Compressed artifacts

A plain filesystem image is not compressed once it is in object storage, and an
ext4 one is not even sparse. `range build --output dev.range` packs the image
into a [Range artifact](ARTIFACT_FORMAT.md) with this layout:

- 1 MiB logical chunks, each zstd-compressed independently
- all-zero chunks, not stored at all
- identical chunks, stored once
- a compressed index at the end

Opening one costs two small range reads. Everything above `ReadAt` (the block cache, profiles,
NBD, the filesystem, overlay) is unchanged and still sees a plain disk image.

Setup, measured 23 September 2026:

- Host: one m6i.2xlarge in us-east-1.
- Environment: one dev environment (Ubuntu, Go, Node, Python and a Go repository with its module
  cache, 2.1 GB of files).
- Workload: `go build ./...` in that repository.
- Every repetition is cold, with the page cache dropped.
- Bytes come from the host NIC counters.
- Each value is the median of 5.

| | Stored in S3 | Publish | Cold | Cold, bytes | With a profile |
| --- | ---: | ---: | ---: | ---: | ---: |
| ext4 image, 20 GiB sparse | 21.47 GB | 26.6 s | 11.44 s | 417 MB | n/a |
| ext4 in a Range artifact | 619 MB | 1.5 s | 7.42 s | 108 MB | 2.87 s |
| EROFS image | 2.38 GB | 3.5 s | 9.03 s | 406 MB | n/a |
| EROFS in a Range artifact (default) | 613 MB | 1.5 s | 6.47 s | 89 MB | 2.80 s |

The compressed artifact is not a trade. Against the plain EROFS image, it stores 3.9x less,
moves a fifth of the bytes and finishes sooner. It finishes sooner because the bytes it does
move decompress faster than the extra bytes would have arrived. Packing costs 8.2 s for this
environment (ext4: 18.2 s). Whoever publishes pays it once. With a learned profile, the two
filesystems are within noise of each other. Cold, EROFS is 13% faster and moves 18% fewer bytes.

`range shell s3://bucket/dev.range` is the same command as before. Range detects the format from
the object's first eight bytes, so raw images keep working.

## Against the other lazy loaders

Downloading first is the wrong thing to beat. SOCI, eStargz and Nydus all fetch on demand too,
so that is the comparison that decides whether Range is interesting.

One m6i.2xlarge in us-east-1 runs every arm. A second instance builds the image once. It pushes
the image to ECR as plain OCI, eStargz, SOCI, and eStargz with a prioritized-files list recorded
from this same workload. It also pushes the image as a Range artifact to S3.

The image is Ubuntu, Go, Node, Python and a cloned repo with its module cache (638 MB
compressed). The workload is `go build ./...` in that repo. Every repetition starts cold, with
the snapshotter state wiped and the page cache dropped. Bytes are the host NIC counters, so no
arm can hide traffic. The table shows the median of five, arms interleaved, measured
23 September 2026:

| Approach | Time to the build finishing | Bytes over the wire |
| --- | ---: | ---: |
| Eager pull through CRI | 21.46 s | 652 MB |
| SOCI, lazy, through CRI | 9.23 s | 37 MB pull, 202 MB in total |
| eStargz, lazy | 10.71 s | 102 MB |
| eStargz, lazy + prioritized files | 7.59 s | 101 MB |
| **Range, cold** | **7.33 s** | **89 MB** |
| **Range + learned profile** | **2.88 s** | **90 MB** |

The last three rows come from a second interleaved run on the same host. There was a second run
because the first eStargz image with prioritized files was barely faster than plain eStargz
(10.04 s). A second image, optimized with the identical command, gave the 7.59 s above. In the
first run, Range took 7.53 s cold and 2.92 s learned.

Cold, Range is level with the fastest of the container loaders and moves the fewest bytes. With
a learned profile, it finishes 2.6x sooner than eStargz with its prioritized files. An earlier
benchmark (20 September) measured eStargz with prioritized files at 5.75 s on a similar image.
That result did not reproduce here. The numbers above are the ones measured side by side.

That earlier benchmark used the old raw ext4 format. There, Range was the slowest lazy loader:
12.66 s cold at 529 MB, and 5.33 s learned. The compressed EROFS artifact was built to fix that.

What is left is not speed. The difference is that Range needs no registry, no daemon, no
snapshotter and no CRI. The artifact is a file in a bucket you already own, and the profile is
a JSON file you can copy.

The difference shows up in setup. **SOCI only lazy-loads under CRI.** Driven from `nerdctl` or
`ctr`, SOCI cannot get the image reference it needs. It logs `unable to get image
ref from labels` and silently pulls the whole image. Measuring SOCI took containerd's CRI plugin,
crictl, CNI plugins and `disable_snapshot_annotations = false`. eStargz's lazy fetch works only
once the snapshotter can find registry credentials of its own.

## When the environment changes

The honest objection to all of the above is "just bake an AMI". Pre-baking wins while the
environment stands still. This section measures what happens when it does not. The test added
one dependency to a Go workspace (`go get golang.org/x/text@v0.14.0`). It then shipped the same
change to five fresh m7g.2xlarge workers two ways.

| From the change to five workers building | Re-bake an AMI | Rebuild the artifact |
| --- | ---: | ---: |
| Make it distributable | 1082.4 s (create-image → available) | **71.4 s** |
| Launch → all five finished the build | 89.6 s | **28.7 s** |
| **Total** | 1172.0 s | **100.1 s** |

Measured 23 September 2026. The Range pipeline has four steps:

1. The change itself (`go get`, 5.4 s).
2. Copying the machine's files into a build tree (9.3 s).
3. `range build` (54.5 s, a 4.05 GB EROFS image stored as 1.19 GB).
4. `range publish` (2.2 s).

On 20 September, with ext4 images padded to 100 GiB, the same pipeline took 220.5 s, 85 s of it
uploading zeros.

The Range workers booted a 30 GB image that knows nothing about the environment. They read the
new environment directly from S3: ready in 0.48–0.79 s, 90 MB each, 452 MB across the fleet.
All ten workers completed the build successfully.

One thing in that table is not flattering framing and is worth stating plainly. The AMI arm's
workers reached their user-data 75 s after launch, against 18 s for Range's workers. This is
because an 80 GB root volume restored from a fresh snapshot hydrates lazily from S3 on first
touch. Range reads from S3 too. It does not pretend to be a local disk first. The benchmark
times each arm from its own launch.

A profile trained on the old version still helps the new one, partially. This result was
measured on 20 September with the ext4 format and not re-run. The v1 profile covered 57.94% of
v2's working set (Jaccard 0.3887). Replaying it against v2 gave 0.35 s to ready, against 0.85 s
cold and 0.28 s with v2's own profile. That is about 59% of the benefit, at the cost of
~196 MB of prefetch that v2 did not want.

Range discards a profile on an identity change rather than replaying it. That is the safe
default. These numbers show what carrying it anyway would buy.
