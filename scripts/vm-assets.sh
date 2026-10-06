#!/bin/bash
# Builds the files Range boots its own Linux VM from: a kernel, the modules
# the VM loads, and a static busybox. Apple's Virtualization.framework boots
# them on a Mac, QEMU with KVM on Linux. The output is one archive for an
# architecture, published as a release asset; Range downloads it once and
# checks it against the SHA-256 in internal/vm/assets.go.
#
#   scripts/vm-assets.sh arm64|amd64 [OUTDIR]
#
# The inputs are pinned by SHA-256. Debian drops old kernels from its pool, so
# moving to a newer kernel means updating the URLs and the checksums together.
set -euo pipefail
arch=${1:?usage: vm-assets.sh arm64|amd64 [OUTDIR]}
out=${2:-dist/vm}

version=6.12.107
case $arch in
arm64)
  kernel_sha=26e8cba299e701c5e7d0d35ffabf83cf8e315e594a834a3b8832e03a0f9787cf
  busybox_arch=aarch64
  busybox_sha=ee469aee2958feffd7f64dd96655704025ff60af614f77a1d7323dc237c34da2
  ;;
amd64)
  kernel_sha=e7124608fd6bbfa1ab73149dc67ecc82d098597d75cf1e540291d8ca71140fe0
  busybox_arch=x86_64
  busybox_sha=488ad6efd04b5a722719e79f8e0dcc2c24afd6758867af3ce41b04839e60c74b
  ;;
*)
  echo "unknown architecture $arch" >&2
  exit 1
  ;;
esac
kernel_version=$version+deb13-cloud-$arch
kernel_url=https://deb.debian.org/debian/pool/main/l/linux/linux-image-$kernel_version-unsigned_$version-1_$arch.deb
busybox_url=https://dl-cdn.alpinelinux.org/alpine/v3.22/main/$busybox_arch/busybox-static-1.37.0-r20.apk

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
# Virtualization.framework boots an uncompressed arm64 Image, which is what
# Debian ships for arm64. QEMU boots the x86 bzImage as it is.
cp "$work/deb/boot/vmlinuz-$kernel_version" "$work/vm/kernel"
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
