# Development

Run from this directory:

```bash
go test -race ./...
go vet ./...
gofmt -l .
go build -o bin/range ./cmd/range
GOOS=linux GOARCH=arm64 go build -o bin/range-linux-arm64 ./cmd/range
```

On macOS, when Go is installed, `range` cross-compiles its own Linux build for the VM from
this tree. A released binary finds `range-linux-<arch>` beside itself instead. The Mac build
uses cgo for Virtualization.framework, so it needs the Xcode command line tools. Range signs
a copy of itself for the VM on first use, so `go build` and `go run` work as they are.

The VM's kernel, modules and busybox come from one archive. `scripts/vm-assets.sh` builds
it from pinned Debian and Alpine packages, reproducibly. Publish it as a release asset and
put its SHA-256 in `internal/vm/assets.go`. `RANGE_VM_ASSETS` points at a local copy.

## Layout

```
cmd/range/            the CLI: one file per command
internal/
  object/             byte ranges from S3, HTTP and files; object identity
  artifact/           Range Artifact v1: header, index, reading, packing
  cache/              block caches in memory and on disk
  profile/            working-set profiles and their recorder
  core/               the block layer: Config and Reader
    coretest/         opens real Readers for other packages' tests
  nbd/                NBD server, client and kernel attachment
  erofs/              the EROFS writer
    erofstest/        reads images back, and mounts them with the kernel
  environment/        /etc/range/environment.json
  oci/                registry client, layers, platforms, layer tree -> EROFS,
                      and images read lazily from their registry
  gzindex/            gzip read from the middle: checkpoints and chunk hashes
  zstdindex/          zstd read by frame, or decoded forward
  virtual/            EROFS images whose file data lives elsewhere
  hub/                Hugging Face repositories as images
  image/              building an environment's filesystem image
  session/            runtimes (Linux, Range's VM, Lima), sessions, namespaces
  vm/                 the Mac's Linux VM: assets, initramfs, vsock, boot
  publish/            multipart upload to S3
  tool/               the host programs Range runs
  bytesize/           parsing and printing sizes
  rangetest/          test helpers shared across packages
```

`cmd/range` is the only package that ties the others together, and nothing in
`internal/` is a stable API. Every package's doc comment says what it owns. The website is a separate static site in `../website`.

Contracts: [ARTIFACT_FORMAT.md](ARTIFACT_FORMAT.md), [CLI.md](CLI.md), [PROFILE_FORMAT.md](PROFILE_FORMAT.md),
[ARCHITECTURE.md](ARCHITECTURE.md).
