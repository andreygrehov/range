#!/bin/sh
# Applies a DHCP lease: address, default route, resolvers.
case "$1" in
deconfig)
  ip link set "$interface" up
  ip addr flush dev "$interface"
  ;;
bound | renew)
  ip addr flush dev "$interface"
  ip addr add "$ip/${mask:-24}" dev "$interface"
  for gateway in $router; do
    ip route replace default via "$gateway" dev "$interface"
    break
  done
  : > /etc/resolv.conf
  for server in $dns; do
    echo "nameserver $server" >> /etc/resolv.conf
  done
  ;;
esac
