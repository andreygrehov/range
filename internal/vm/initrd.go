package vm

import (
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

//go:embed init.sh
var initScript []byte

//go:embed udhcpc.sh
var dhcpScript []byte

// Initrd returns a gzip'd initramfs holding the init script, busybox, the
// modules from assets and guest, the Linux build of Range. It is built once
// for each guest binary, known by its path, size and time, and kept beside
// the assets; older ones are removed.
func Initrd(assets, guest string) (string, error) {
	info, err := os.Stat(guest)
	if err != nil {
		return "", fmt.Errorf("the guest binary: %w", err)
	}
	key := sha256.New()
	key.Write(initScript)
	key.Write(dhcpScript)
	fmt.Fprintf(key, "%s|%d|%d", guest, info.Size(), info.ModTime().UnixNano())
	path := filepath.Join(assets, "initrd-"+hex.EncodeToString(key.Sum(nil))[:16]+".gz")
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	guestData, err := os.ReadFile(guest)
	if err != nil {
		return "", fmt.Errorf("read the guest binary: %w", err)
	}

	order, err := os.ReadFile(filepath.Join(assets, "modules", "order"))
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(assets, ".initrd-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	gz, _ := gzip.NewWriterLevel(tmp, gzip.BestSpeed)
	w := newCPIO(gz)
	for _, dir := range []string{"bin", "dev", "etc", "lib", "lib/modules", "proc", "run", "sys", "tmp", "root"} {
		w.dir(dir)
	}
	w.device("dev/console", 5, 1)
	w.file("init", 0o755, initScript)
	w.file("etc/udhcpc.sh", 0o755, dhcpScript)
	w.file("etc/resolv.conf", 0o644, nil)
	w.file("bin/range", 0o755, guestData)
	if err := w.fileFrom("bin/busybox", 0o755, filepath.Join(assets, "busybox")); err != nil {
		return "", err
	}
	w.file("lib/modules/order", 0o644, order)
	scanner := bufio.NewScanner(strings.NewReader(string(order)))
	for scanner.Scan() {
		module := strings.TrimSpace(scanner.Text())
		if module == "" {
			continue
		}
		if err := w.fileFrom("lib/modules/"+module+".ko", 0o644, filepath.Join(assets, "modules", module+".ko")); err != nil {
			return "", err
		}
	}
	w.trailer()
	if w.err != nil {
		return "", w.err
	}
	if err := gz.Close(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	old, _ := filepath.Glob(filepath.Join(assets, "initrd-*.gz"))
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	for _, stale := range old {
		os.Remove(stale)
	}
	return path, nil
}

// cpioWriter writes the "newc" cpio format the kernel unpacks an initramfs
// from: a 110-byte ASCII header, the name, and the data, each padded to four
// bytes.
type cpioWriter struct {
	w     io.Writer
	inode int
	err   error
}

func newCPIO(w io.Writer) *cpioWriter { return &cpioWriter{w: w, inode: 1} }

func (c *cpioWriter) entry(name string, mode uint32, major, minor uint32, data []byte) {
	if c.err != nil {
		return
	}
	c.inode++
	nlink := 1
	if mode&0o170000 == 0o040000 {
		nlink = 2
	}
	header := fmt.Sprintf("070701%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X",
		c.inode, mode, 0, 0, nlink, 0, len(data), 0, 0, major, minor, len(name)+1, 0)
	c.write([]byte(header))
	c.write(append([]byte(name), 0))
	c.pad(len(header) + len(name) + 1)
	c.write(data)
	c.pad(len(data))
}

func (c *cpioWriter) dir(name string) { c.entry(name, 0o040755, 0, 0, nil) }
func (c *cpioWriter) device(name string, major, minor uint32) {
	c.entry(name, 0o020600, major, minor, nil)
}
func (c *cpioWriter) file(name string, perm uint32, data []byte) {
	c.entry(name, 0o100000|perm, 0, 0, data)
}
func (c *cpioWriter) trailer() { c.entry("TRAILER!!!", 0, 0, 0, nil) }

func (c *cpioWriter) fileFrom(name string, perm uint32, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	c.file(name, perm, data)
	return nil
}

func (c *cpioWriter) write(b []byte) {
	if c.err == nil {
		_, c.err = c.w.Write(b)
	}
}

func (c *cpioWriter) pad(n int) {
	if rem := n % 4; rem != 0 {
		c.write(make([]byte, 4-rem))
	}
}
