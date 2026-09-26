// Package image builds an environment's filesystem image. The default is
// EROFS, written in Go from a directory or a container image, which needs no
// mkfs, no root and no loop device. The ext4 path keeps the older behaviour,
// where mkfs.ext4 -d populates a filesystem from a directory; the minimum
// image size applies only there, since an EROFS image is sized to its
// contents.
package image
