#!/bin/sh
# Times Go's resolver against the namespaces from netns/setup.sh, without and with
# errqueue. Needs root. The router and server rate-limit ICMP errors per source,
# so each case starts on a refilled budget: a run that follows another too
# closely loses errors to the limit and waits out the timeout instead.
set -eu
bin=${1:-./resolver}
for server in 10.0.9.1:53 '[fd00:9::1]:53' 10.0.2.2:53; do
	for mode in errqueue plain; do
		sleep 7
		flag=
		[ "$mode" = errqueue ] && flag=-errqueue
		printf '%-16s %-8s ' "$server" "$mode"
		"$bin" -server "$server" $flag
	done
done
