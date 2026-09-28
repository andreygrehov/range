// Package oci builds environments straight from container images, with no
// container runtime installed:
//
//	range build --from-oci ubuntu:24.04 -o dev.range
//
// Only what an image build needs is implemented: anonymous pull, manifest and
// platform selection, digest verification of the manifest, the config and
// every layer, and layer application with whiteouts. Tree applies layers in
// memory and writes EROFS from them, so ownership comes from the layer headers
// and nothing is unpacked onto the host; ExtractLayer unpacks onto disk for
// the ext4 path.
package oci
