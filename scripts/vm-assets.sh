#!/bin/bash
# Builds the files Range boots its own Linux VM from on a Mac: a kernel, the
# modules the VM loads, and a static busybox. The output is one archive,
# published as a release asset; Range downloads it once and checks it against
# the SHA-256 in internal/vm/assets.go.
#
#   scripts/vm-assets.sh [OUTDIR]
#
# The inputs are pinned by SHA-256. Debian drops old kernels from its pool, so
# moving to a newer kernel means updating the URL and the checksum together.
set -euo pipefail
out=${1:-dist/vm}

kernel_url=https://deb.debian.org/debian/pool/main/l/linux/linux-image-6.12.107+deb13-cloud-arm64-unsigned_6.12.107-1_arm64.deb
kernel_sha=26e8cba299e701c5e7d0d35ffabf83cf8e315e594a834a3b8832e03a0f9787cf
kernel_version=6.12.107+deb13-cloud-arm64
busybox_url=https://dl-cdn.alpinelinux.org/alpine/v3.22/main/aarch64/busybox-static-1.37.0-r20.apk
busybox_sha=ee469aee2958feffd7f64dd96655704025ff60af614f77a1d7323dc237c34da2

# The order the VM loads them in, each after what it depends on.
modules="drivers/char/hw_random/virtio-rng drivers/block/virtio_blk lib/libcrc32c fs/erofs/erofs
fs/overlayfs/overlay net/core/failover drivers/net/net_failover drivers/net/virtio_net
net/vmw_vsock/vsock net/vmw_vsock/vmw_vsock_virtio_transport_common net/vmw_vsock/vmw_vsock_virtio_transport"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
fetch() {
  curl -fsSL -o "$work/$2" "$1"
  echo "$3  $work/$2" | shasum -a 256 -c - >/dev/null || { echo "checksum mismatch: $1" >&2; exit 1; }
}
fetch "$kernel_url" kernel.deb "$kernel_sha"
fetch "$busybox_url" busybox.apk "$busybox_sha"

mkdir -p "$work/deb" "$work/apk" "$work/vm/modules"
(cd "$work/deb" && ar x ../kernel.deb && tar -xf data.tar.*)
tar -xzf "$work/busybox.apk" -C "$work/apk" 2>/dev/null
# The kernel must be an uncompressed arm64 Image: Virtualization.framework
# boots nothing else.
cp "$work/deb/boot/vmlinuz-$kernel_version" "$work/vm/Image"
cp "$work/apk/bin/busybox.static" "$work/vm/busybox"
for m in $modules; do
  xz -dc "$work/deb/usr/lib/modules/$kernel_version/kernel/$m.ko.xz" > "$work/vm/modules/$(basename "$m").ko"
done
echo "$modules" | xargs -n1 basename > "$work/vm/modules/order"
echo "$kernel_version" > "$work/vm/kernel-version"

mkdir -p "$out"
name="range-vm-$kernel_version.tar.gz"
# A fixed order, owner and time keep the archive, and its checksum, reproducible.
find "$work/vm" -exec touch -t 202601010000 {} +
(cd "$work/vm" && find . -type f | LC_ALL=C sort |
  tar -cf - --uid 0 --gid 0 --uname root --gname root -T - | gzip -9 -n) > "$out/$name"
shasum -a 256 "$out/$name"
