package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/andreygrehov/range/internal/artifact"
	"github.com/andreygrehov/range/internal/bytesize"
	"github.com/andreygrehov/range/internal/image"
	"github.com/andreygrehov/range/internal/oci"
	"github.com/andreygrehov/range/internal/session"
)

func commandImage(args []string) error {
	if len(args) == 0 || args[0] != "build" {
		return errors.New("image: expected \"build\"")
	}
	fs := flag.NewFlagSet("image build", flag.ContinueOnError)
	size := fs.String("size", "", "logical size, ext4 only; EROFS is sized to its contents")
	fsType := fs.String("fs", "erofs", "filesystem of the environment: erofs (default) or ext4")
	output := fs.String("output", "", "path to write")
	fs.StringVar(output, "o", "", "path to write (short form)")
	format := fs.String("format", "range", "artifact format: range (compressed, default) or the bare filesystem image")
	chunk := fs.String("chunk-size", "1MiB", "logical chunk size of a compressed artifact")
	fromOCI := fs.String("from-oci", "", "build from a container image, e.g. ubuntu:24.04")
	platformFlag := fs.String("platform", "", "platform to pull with --from-oci, e.g. linux/amd64 (default: this host)")
	flags, rest := splitArgs(fs, args[1:])
	if err := fs.Parse(flags); err != nil {
		return err
	}
	platform := oci.HostPlatform()
	if *platformFlag != "" {
		if *fromOCI == "" {
			return errors.New("--platform chooses which image --from-oci pulls; a directory build is whatever the directory holds")
		}
		parsed, err := oci.ParsePlatform(*platformFlag)
		if err != nil {
			return err
		}
		platform = parsed
	}
	if len(rest) == 0 && *fromOCI == "" {
		return errors.New("image build: give a <rootfs-dir> or --from-oci <image>")
	}
	if *fsType != "erofs" && *fsType != "ext4" {
		return fmt.Errorf("--fs must be erofs or ext4, got %q", *fsType)
	}
	if *size == "" {
		*size = "20GiB"
	}
	bytes, err := bytesize.Parse(*size)
	if err != nil {
		return err
	}
	source := ""
	if len(rest) > 0 {
		source = rest[0]
	}
	started := time.Now()
	packed, err := wantsPacking(*format, *output)
	if err != nil {
		return err
	}
	chunkSize, err := bytesize.Parse(*chunk)
	if err != nil {
		return err
	}
	suffix := ".range"
	if !packed {
		suffix = ".img"
	}
	if *fsType == "erofs" {
		// Written in Go from the tar headers or the directory, on any host and
		// without root, so nothing here needs the Linux VM.
		req := image.BuildRequest{Rootfs: source, Output: *output, FromOCI: *fromOCI,
			FS: "erofs", Align: chunkSize, Platform: platform}
		if req.Output == "" {
			name := *fromOCI
			if name == "" {
				name = filepath.Base(strings.TrimRight(source, "/"))
			}
			req.Output = strings.NewReplacer("/", "-", ":", "-").Replace(name) + suffix
		}
		return buildThenPack(req, packed, chunkSize, started)
	}
	// ext4 unpacks onto a real filesystem and relies on chown to record who
	// owns each file. Without root those calls fail, and the image would
	// silently belong to whoever ran the build.
	if *fromOCI != "" && runtime.GOOS == "linux" && os.Geteuid() != 0 {
		return errors.New("--fs ext4 from an image needs root to keep file ownership; " +
			"run with sudo, or drop --fs ext4 to build EROFS without root")
	}
	if *fromOCI != "" && runtime.GOOS != "linux" {
		// The guest pulls and builds in one step; nothing to stage on the host.
		req := image.BuildRequest{Size: bytes, Output: *output, FromOCI: *fromOCI,
			FS: *fsType, Align: chunkSize, Platform: platform}
		if req.Output == "" {
			req.Output = strings.NewReplacer("/", "-", ":", "-").Replace(*fromOCI) + suffix
		}
		return buildThenPack(req, packed, chunkSize, started)
	}
	if *fromOCI != "" {
		staging, err := os.MkdirTemp("", "range-oci-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(staging)
		fmt.Fprintf(os.Stderr, "Pulling %s\n", *fromOCI)
		if err := oci.FetchRootfs(context.Background(), *fromOCI, staging, platform); err != nil {
			return fmt.Errorf("pull %s: %w", *fromOCI, err)
		}
		for _, dir := range []string{"proc", "sys", "dev", "tmp", "etc"} {
			os.MkdirAll(filepath.Join(staging, dir), 0o755)
		}
		// 0o1777 would be wrong here: Go's FileMode keeps the sticky bit in
		// os.ModeSticky, and a literal 0o1000 is silently dropped, leaving a
		// world-writable /tmp in which anyone can delete anyone's files.
		os.Chmod(filepath.Join(staging, "tmp"), 0o777|os.ModeSticky)
		source = staging
	}
	req := image.BuildRequest{Rootfs: source, Size: bytes, Output: *output, FromOCI: *fromOCI,
		FS: *fsType, Align: chunkSize, Platform: platform}
	if req.Output == "" {
		name := *fromOCI
		if name == "" {
			name = filepath.Base(strings.TrimRight(source, "/"))
		}
		req.Output = strings.NewReplacer("/", "-", ":", "-").Replace(name) + suffix
	}
	return buildThenPack(req, packed, chunkSize, started)
}

// buildThenPack runs the platform builder and, unless raw output was asked for,
// packs the disk image it produced into a Range artifact. The raw image is an
// intermediate either way; it is built beside the target and removed.
func buildThenPack(req image.BuildRequest, packed bool, chunkSize int64, started time.Time) error {
	target := req.Output
	if packed {
		req.Output = target + ".raw"
		defer os.Remove(req.Output)
	}
	build := session.Select().BuildImage
	if req.FS != "ext4" {
		build = func(_ context.Context, r image.BuildRequest) error { return image.BuildNative(r) }
	}
	if err := build(context.Background(), req); err != nil {
		return err
	}
	if !packed {
		return reportBuiltImage(req.Output, started)
	}
	st, err := artifact.Pack(req.Output, target, chunkSize)
	if err != nil {
		return err
	}
	return reportPackedArtifact(target, st, started)
}

// wantsPacking decides between the compressed artifact and a raw disk image.
// "auto" follows the extension, so ".range" means compressed and anything else
// keeps the old behaviour.
// wantsPacking decides between the compressed artifact and a raw disk image.
// The compressed artifact is the default; raw is kept for debugging and for
// anything that needs to loop-mount the file directly. "auto" follows the
// output extension, for a caller that wants the old behaviour.
func wantsPacking(format, output string) (bool, error) {
	switch strings.ToLower(format) {
	case "range", "":
		return true, nil
	case "raw":
		return false, nil
	case "auto":
		return strings.HasSuffix(strings.ToLower(output), ".range"), nil
	default:
		return false, fmt.Errorf("format must be range, raw or auto, got %q", format)
	}
}

func reportPackedArtifact(target string, st artifact.PackStats, started time.Time) error {
	ratio := 0.0
	if st.Stored > 0 {
		ratio = float64(st.Logical) / float64(st.Stored)
	}
	fmt.Printf("Built %s\n", target)
	fmt.Printf("  Logical size      %s\n", bytesize.Format(st.Logical))
	fmt.Printf("  Stored size       %s\n", bytesize.Format(st.Stored))
	fmt.Printf("  Compression       %.1fx\n", ratio)
	fmt.Printf("  Chunks            %d stored, %d all-zero, %d deduplicated (of %d)\n",
		st.Chunks-st.Zero-st.Deduped, st.Zero, st.Deduped, st.Chunks)
	fmt.Printf("  Index             %s\n", bytesize.Format(st.IndexBytes))
	fmt.Printf("  Took              %s\n", roundedSeconds(time.Since(started)))
	fmt.Printf("\nUse it with:\n  range shell %s\n", target)
	return nil
}

func reportBuiltImage(output string, started time.Time) error {
	info, err := os.Stat(output)
	if err != nil {
		return err
	}
	fmt.Printf("Built %s\n", output)
	fmt.Printf("  Logical size      %s\n", bytesize.Format(info.Size()))
	fmt.Printf("  On disk           %s\n", bytesize.Format(diskUsage(output)))
	fmt.Printf("  Took              %s\n", roundedSeconds(time.Since(started)))
	fmt.Printf("\nUse it with:\n  range shell %s\n", output)
	return nil
}

// diskUsage reports blocks actually allocated, which is far less than the
// logical size for a sparse image.
func diskUsage(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return int64(stat.Blocks) * 512
	}
	return info.Size()
}
