// Package session turns a remote artifact into a usable Linux shell:
//
//	artifact -> nbd device -> read-only EROFS -> overlay -> namespaces -> shell
//
// A Runtime does the kernel half for the host it runs on: Native on Linux, and
// Lima on macOS, where a Linux VM runs the environment and reaches the
// artifact over NBD through an ssh reverse tunnel while the cache, profiles
// and credentials stay on the Mac. Mount, overlay and namespace work is
// delegated to util-linux rather than reimplemented through raw syscalls.
package session
