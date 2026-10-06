#!/bin/busybox sh
# PID 1 of the VM Range boots on a Mac. It brings up what the Linux half of a
# session expects - /proc, /sys, /dev, the modules, a network - and hands over
# to range, which mounts the image from /dev/vda exactly as on Linux.
/bin/busybox --install -s /bin
export PATH=/bin
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs dev /dev
mkdir -p /dev/pts /dev/shm
mount -t devpts -o ptmxmode=0666 devpts /dev/pts
mount -t tmpfs -o mode=1777 tmp /tmp
mount -t tmpfs -o mode=755 run /run
# The network comes up first: DHCP then runs while the rest loads.
for module in failover net_failover virtio_net; do
  insmod "/lib/modules/$module.ko" || echo "range-vm: could not load $module" >&2
done
ip link set lo up
# The hook fills /etc/resolv.conf in place, so the copy bound into the
# environment sees it; range waits for it before starting the workload.
(
  while [ ! -e /sys/class/net/eth0 ]; do sleep 0.01; done
  udhcpc -i eth0 -s /etc/udhcpc.sh -q -t 30 -T 1 >/dev/null 2>&1
) &
while read -r module; do
  case "$module" in failover | net_failover | virtio_net) continue ;; esac
  insmod "/lib/modules/$module.ko" || echo "range-vm: could not load $module" >&2
done < /lib/modules/order
exec /bin/range __vm
