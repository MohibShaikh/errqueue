#!/usr/bin/env bash
# Kernel behaviour the error-queue code depends on. Each probe prints one PASS/FAIL line
# and exits 0/1; exit 2 means setup failed. Loopback only, no root.
set -u
cd "$(dirname "$0")"
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT
fails=0

probe() { # name source claim [cflags...]
  local name=$1 src=$2 claim=$3; shift 3
  if ! gcc -Wall -Wextra -Werror "$@" -o "$out/$name" "$src"; then
    echo "UNVERIFIED $name: compile failed"; fails=$((fails + 1)); return
  fi
  local line
  line=$(timeout 5 "$out/$name")
  local rc=$?
  echo "$line  [$name] $claim"
  [[ $rc -eq 0 ]] || fails=$((fails + 1))
}

echo "kernel $(uname -r) $(uname -m)"
probe v4_unreach v4_unreach.c "IPv4 IP_RECVERR: send to a closed port queues origin ICMP, type 3 code 3, ECONNREFUSED"
probe v6_unreach v6_unreach.c "IPv6 IPV6_RECVERR: send to a closed port queues origin ICMP6, type 1 code 4, ECONNREFUSED"
probe v6_local_mtu v6_local_mtu.c "IPv6 IPV6_MTU=1280 + DONTFRAG: 1400-byte send fails EMSGSIZE, queues LOCAL origin with ee_info=1280"
probe trunc trunc.c "MSG_ERRQUEUE read with short buffers sets MSG_TRUNC and MSG_CTRUNC and still dequeues the entry"
probe absorb quinn.c "IP_RECVERR: after 1 queued error the next send to a live peer fails ECONNREFUSED; the retry is delivered" \
  -DRECVERR=1 -DNERR=1 -DNDRAIN=0 '-DEXPECT=(fails == 1 && first == ECONNREFUSED && del == 1)'
probe three_queued quinn.c "IP_RECVERR: 3 queued errors, none read: exactly 1 following send fails" \
  -DRECVERR=1 -DNERR=3 -DNDRAIN=0 '-DEXPECT=(fails == 1 && first == ECONNREFUSED && del == 1)'
probe drain_all quinn.c "IP_RECVERR: reading all 3 queued errors before sending: no send fails" \
  -DRECVERR=1 -DNERR=3 -DNDRAIN=-1 '-DEXPECT=(drained == 3 && fails == 0 && del == 1)'
probe drain_partial quinn.c "IP_RECVERR: reading 1 of 3 queued errors re-arms the error: the next send fails once" \
  -DRECVERR=1 -DNERR=3 -DNDRAIN=1 '-DEXPECT=(drained == 1 && fails == 1 && first == ECONNREFUSED && del == 1)'
probe control quinn.c "without IP_RECVERR: an earlier port unreachable does not fail the next send" \
  -DRECVERR=0 -DNERR=1 -DNDRAIN=0 '-DEXPECT=(fails == 0 && del == 1)'

exit $((fails > 0))
