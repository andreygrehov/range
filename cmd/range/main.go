package main

import (
	"fmt"
	"log"
	"os"

	"github.com/andreygrehov/range/internal/session"
)

const usage = `range — use a remote environment before downloading it

Usage:
  range build --from-oci IMAGE [-o FILE]                 build an environment
  range build <dir> [-o FILE]                            build one from a directory
  range publish <artifact> <uri>                         upload it, once
  range shell <uri>                                      open a shell inside it
  range run <uri> -- <command>                           run one command inside it
  range serve <uri>                                      serve it over local HTTP, by range

Nothing is downloaded: reads are served from the object as they happen.

  range inspect <uri>                      identity, size and cache state
  range stats [uri]                        transfer and cache statistics
  range cache stats | clear [uri]          local cache usage, or delete blocks
  range profile show|export|import|clear   the learned working set
  range reset [uri]                        forget cached blocks and profiles
  range doctor                             what this host can run
  range debug ...                          raw reads, NBD, the builder

Build flags:
  --from-oci IMAGE      build from a container image, no container runtime needed
  --platform linux/ARCH which image --from-oci pulls (default: this host's)
  --fs erofs|ext4       filesystem inside it (default erofs: pure Go, no root)
  --size SIZE           logical size, ext4 only (default 20GiB); EROFS fits its contents
  --output, -o FILE     where to write it (default <image>.range)
  --format range|raw    compressed Range artifact (default) or the bare filesystem image
  --chunk-size SIZE     logical chunk size of the artifact (default 1MiB)

Common flags:
  --block-size=1MiB     block granularity for remote reads
  --cache-size=10GiB    local disk cache limit
  --memory-cache=64MiB  in-process cache limit
  --max-range=8MiB      largest coalesced remote request
  --request-timeout=30s ceiling on one remote request
  --prefetch=on|off     sequential prefetch
  --cache-dir=PATH      cache location
  --trace=PATH          write a JSONL access trace
  --profile=off|record|auto  working-set profile handling (default auto)
  --prefetch-limit=256MiB    ceiling on profile prefetch

URIs may be s3://bucket/key, https://host/path, or a local file path.
`

func main() {
	log.SetFlags(0)
	log.SetPrefix("")
	if err := run(os.Args[1:]); err != nil {
		log.Fatalf("range: %v", err)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return nil
	}
	switch args[0] {
	// The environment surface. Nothing here mentions a block device.
	case "build":
		return commandImage(append([]string{"build"}, args[1:]...))
	case "shell":
		return commandShell(args[1:])
	case "run":
		return commandRun(args[1:])
	case "publish":
		return commandPublish(args[1:])
	case "inspect":
		return commandInfo(args[1:])
	case "serve":
		return commandServe(args[1:])

	// Operating the cache and the learned profiles.
	case "stats":
		return commandStats(args[1:])
	case "cache":
		return commandCache(args[1:])
	case "profile":
		return commandProfile(args[1:])
	case "reset":
		return commandReset(args[1:])
	case "doctor":
		return commandDoctor(args[1:])

	// The plumbing, kept but out of the way.
	case "debug":
		return commandDebug(args[1:])

	// Invoked by range itself, never typed.
	case session.ChildCommand:
		return session.RunChild(args[1:])
	case session.GuestCommand:
		return commandGuest(args[1:])

	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	default:
		fmt.Print(usage)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// commandDebug holds everything that exposes how Range works rather than what
// it is for: raw reads, the NBD export and the builder.
func commandDebug(args []string) error {
	if len(args) == 0 {
		fmt.Print(debugUsage)
		return nil
	}
	switch args[0] {
	case "read":
		return commandRead(args[1:])
	case "cat":
		return commandCat(args[1:])
	case "nbd":
		return commandNBD(args[1:])
	case "info":
		return commandInfo(args[1:])
	case "image":
		return commandImage(args[1:])
	case "help", "-h", "--help":
		fmt.Print(debugUsage)
		return nil
	default:
		fmt.Print(debugUsage)
		return fmt.Errorf("unknown debug command %q", args[0])
	}
}

const debugUsage = `range debug — the plumbing

Usage:
  range debug read <uri> --offset N --length N   read a byte range to stdout
  range debug cat <uri>                          stream the whole artifact to stdout
  range debug nbd serve <uri> [--addr host:port] export the artifact over NBD (TCP)
  range debug nbd attach <uri> <device>          attach to /dev/nbdN (Linux)
  range debug info <uri>                         artifact identity and cache state
  range debug image build ...                    the builder, spelled out

These exist because they are useful when something is wrong. Nothing in the
normal path requires knowing that NBD is involved.
`
