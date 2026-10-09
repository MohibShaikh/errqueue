#!/bin/sh
# Times CoreDNS failover when its first upstream becomes unreachable. Needs root
# and a fresh run of netns/setup.sh, which this script does itself.
#
#   run.sh COREDNS LOAD 4|6
#
# COREDNS is a CoreDNS binary, built with or without coredns.patch. LOAD is
# ./load built. The server namespace runs two upstreams that answer every query
# (whoami); COREDNS forwards to them in order on 127.0.0.1:1053. Four seconds
# into a 12 s load of 50 queries/s, the router gets an unreachable route to the
# first upstream.
set -eu
coredns=$1 load=$2 family=$3
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
tmp=$(mktemp -d)
trap 'kill $(jobs -p) 2>/dev/null; rm -rf "$tmp"' EXIT
printf '.:53 {\n\tbind %s %s\n\twhoami\n}\n' $up2 $up1 >"$tmp/upstream"
printf '.:1053 {\n\tbind 127.0.0.1\n\tforward . %s {\n\t\tpolicy sequential\n\t}\n}\n' "$ups" >"$tmp/forwarder"
ip netns exec eqs "$coredns" -conf "$tmp/upstream" -quiet >/dev/null 2>&1 &
"$coredns" -conf "$tmp/forwarder" -quiet >/dev/null 2>&1 &
sleep 2
(sleep 4 && ip -n eqr route add unreachable $up1/$plen) &
"$load" -server 127.0.0.1:1053 -qps 50 -for 12s
ip netns exec eqr nstat -az | awk '/Icmp6?Out(DestUnreachs|RateLimitHost)/ {printf "%s=%s ", $1, $2} END {print ""}'
