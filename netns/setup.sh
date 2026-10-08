#!/bin/sh
# Builds a router between this namespace and a server namespace, for the tests
# in netns_linux_test.go. Needs root. The router's link to the server has an MTU
# of 1300, and it has unreachable routes for 10.0.9.0/24 and fd00:9::/64.
#
#   this ns  veth c0 ---- c1 [router "eqr"] s1 ---- s0 [server "eqs"]
#            10.0.1.1     10.0.1.2   10.0.2.1        10.0.2.2
#            fd00:1::1    fd00:1::2  fd00:2::1       fd00:2::2
set -eu

ip netns add eqr
ip netns add eqs
ip link add c0 type veth peer name c1 netns eqr
ip -n eqr link add s1 mtu 1300 type veth peer name s0 netns eqs
ip -n eqs link set s0 mtu 1300

ip addr add 10.0.1.1/24 dev c0
ip addr add fd00:1::1/64 dev c0 nodad
ip link set c0 up
ip route add 10.0.0.0/16 via 10.0.1.2
ip route add fd00::/16 via fd00:1::2

ip -n eqr addr add 10.0.1.2/24 dev c1
ip -n eqr addr add fd00:1::2/64 dev c1 nodad
ip -n eqr addr add 10.0.2.1/24 dev s1
ip -n eqr addr add fd00:2::1/64 dev s1 nodad
ip -n eqr link set c1 up
ip -n eqr link set s1 up
ip -n eqr route add unreachable 10.0.9.0/24
ip -n eqr route add unreachable fd00:9::/64
# Keep the default ICMP rate limits, which allow a burst of about five errors per
# source. net.ipv4.icmp_ratelimit=0 empties that burst on kernels before 7.0
# (net/ipv4/inetpeer.c:261), and the router then drops the unreachable silently.
ip netns exec eqr sysctl -qw net.ipv4.ip_forward=1 net.ipv6.conf.all.forwarding=1

ip -n eqs addr add 10.0.2.2/24 dev s0
ip -n eqs addr add fd00:2::2/64 dev s0 nodad
ip -n eqs link set s0 up
ip -n eqs route add default via 10.0.2.1
ip -n eqs route add default via fd00:2::1
