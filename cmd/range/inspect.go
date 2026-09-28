package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/andreygrehov/range/internal/bytesize"
)

func commandInfo(args []string) error {
	r, _, _, err := openFromArgs("info", args, nil)
	if err != nil {
		return err
	}
	defer r.Close()
	fmt.Printf("URI:            %s\n", r.Ident.URI)
	fmt.Printf("Size:           %d\n", r.Ident.Size)
	fmt.Printf("Size human:     %s\n", bytesize.Format(r.Ident.Size))
	fmt.Printf("ETag:           %s\n", orDash(r.Ident.ETag))
	fmt.Printf("Version:        %s\n", orDash(r.Ident.VersionID))
	fmt.Printf("Modified:       %s\n", r.Ident.LastModified.Format(time.RFC3339))
	fmt.Printf("Block size:     %s\n", bytesize.Format(r.BlockSize))
	fmt.Printf("Blocks:         %d\n", r.BlockCount())
	fmt.Printf("Cached:         %s\n", bytesize.Format(r.Disk.Used()))
	fmt.Printf("Cache dir:      %s\n", r.ObjectDir)
	return nil
}

func orDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func commandRead(args []string) error {
	var offset, length int64
	r, _, _, err := openFromArgs("read", args, func(fs *flag.FlagSet) {
		fs.Int64Var(&offset, "offset", 0, "byte offset")
		fs.Int64Var(&length, "length", 4096, "number of bytes")
	})
	if err != nil {
		return err
	}
	defer r.Close()
	if length <= 0 {
		return errors.New("read: --length must be positive")
	}
	buf := make([]byte, length)
	n, err := r.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	_, err = os.Stdout.Write(buf[:n])
	return err
}

func commandCat(args []string) error {
	r, _, _, err := openFromArgs("cat", args, nil)
	if err != nil {
		return err
	}
	defer r.Close()
	_, err = io.Copy(os.Stdout, io.NewSectionReader(r, 0, r.Size()))
	return err
}
