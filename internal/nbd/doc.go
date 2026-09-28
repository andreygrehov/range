// Package nbd speaks the Network Block Device protocol, both sides of it: a
// read-only fixed-newstyle server that exports any io.ReaderAt, a client
// handshake, and the Linux ioctls that hand a connected socket to the kernel's
// nbd driver, so the export appears as /dev/nbdN.
package nbd
