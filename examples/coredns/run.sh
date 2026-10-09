#!/bin/sh
# Times CoreDNS failover when its first upstream becomes unreachable. Needs root
# and a fresh run of netns/setup.sh, which this script does itself.
#
#   run.sh COREDNS LOAD 4|6 [all]
#
# COREDNS is a CoreDNS binary, built with or without coredns.patch. LOAD is
# ./load built. The server namespace runs two upstreams that answer every query
# (whoami); COREDNS forwards to them in order on 127.0.0.1:1053. Four seconds
# into a 12 s load of 50 queries/s, the router gets an unreachable route to the
# first upstream, or with "all" to both, and the script counts the packets
# COREDNS sends upstream from then on. NOLIMIT=1 turns off the router's ICMP rate
# limits that a namespace can change, so every dropped packet gets an error.
set -eu
coredns=$1 load=$2 family=$3 which=${4:-first}
dir=$(cd "$(dirname "$0")" && pwd)
if [ "$family" = 6 ]; then
	up1=fd00:2::3 up2=fd00:2::2 ups='[fd00:2::3]:53 [fd00:2::2]:53' plen=128
else
	up1=10.0.2.3 up2=10.0.2.2 ups='10.0.2.3:53 10.0.2.2:53' plen=32
fi

"$dir/../../netns/setup.sh"
if [ "$family" = 6 ]; then
	ip -n eqs addr add $up1/64 dev s0 nodad
else
	ip -n eqs addr add $up1/24 dev s0
fi
if [ "${NOLIMIT:-}" = 1 ]; then
	ip netns exec eqr sysctl -qw net.ipv4.icmp_ratelimit=0 net.ipv6.icmp.ratelimit=0 \
		net.ipv4.icmp_msgs_per_sec=100000 net.ipv4.icmp_msgs_burst=100000
fi
down=$up1
[ "$which" = all ] && down="$up1 $up2"
tmp=$(mktemp -d)
trap 'kill $(jobs -p) 2>/dev/null; rm -rf "$tmp"' EXIT
printf '.:53 {\n\tbind %s %s\n\twhoami\n}\n' $up2 $up1 >"$tmp/upstream"
printf '.:1053 {\n\tbind 127.0.0.1\n\tforward . %s {\n\t\tpolicy sequential\n\t}\n}\n' "$ups" >"$tmp/forwarder"
ip netns exec eqs "$coredns" -conf "$tmp/upstream" -quiet >/dev/null 2>&1 &
"$coredns" -conf "$tmp/forwarder" -quiet >/dev/null 2>&1 &
sleep 2
(
	sleep 4
	cat /sys/class/net/c0/statistics/tx_packets >"$tmp/tx"
	for a in $down; do ip -n eqr route add unreachable $a/$plen; done
) &
"$load" -server 127.0.0.1:1053 -qps 50 -for 12s
echo "packets sent upstream from 4 s: $(($(cat /sys/class/net/c0/statistics/tx_packets) - $(cat "$tmp/tx")))"
ip netns exec eqr nstat -az | awk '/Icmp6?Out(DestUnreachs|RateLimitHost)/ {printf "%s=%s ", $1, $2} END {print ""}'
