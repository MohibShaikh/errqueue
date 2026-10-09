# errqueue

Reads ICMP errors and local send errors from a UDP socket's error queue on Linux.

An unconnected UDP socket never learns that a peer's port is closed or that a router dropped a packet as too big. A connected one gets at most a bare errno for some of these, with no detail. The kernel does know, and with `IP_RECVERR` set it keeps each error on a per-socket queue that `recvmsg(MSG_ERRQUEUE)` reads. This package sets the options, reads the queue and parses each entry into an `Event` with the errno, ICMP type and code, the reporting host, the original destination, the quoted payload, and the MTU for "packet too big".

## Enabling it changes the socket

Once the queue is on, the kernel also reports every new error on the next ordinary read or send.

- A goroutine blocked in `ReadFrom` returns `ECONNREFUSED` or `EHOSTUNREACH` with no data.
- The next send fails and its datagram is never sent, even when it was going to a different host.

A read loop that treats every error as fatal will stop on the first ICMP error. Drain the queue when a read fails and keep reading. Send through `Send`, which drains and retries once when the send failed with an errno that an ICMP error produces.

`Drain` reads until the queue is empty. Reading only part of it re-arms the pending error, and the next send fails again.

Give each socket one goroutine that drains it. While one goroutine drains, another goroutine's read or send can take the re-armed error and then find the queue empty. `Send` retries in that case anyway. A reader that finds nothing to drain should keep reading.

## Usage

```go
conn, err := net.ListenUDP("udp", nil)
if err != nil {
	log.Fatal(err)
}
if err := errqueue.Enable(conn); err != nil {
	log.Fatal(err)
}

go func() {
	buf := make([]byte, 1500)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if errors.Is(err, net.ErrClosed) {
			return
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return // the read deadline passed; Drain does not reset it
		}
		if err != nil {
			if _, err := errqueue.Drain(conn, handle); err != nil {
				log.Print(err)
			}
			continue // a queued ICMP error failed the read, not a broken socket
		}
		log.Printf("%d bytes from %v", n, from)
	}
}()

err = errqueue.Send(conn, func() error {
	_, err := conn.WriteToUDP(msg, dst)
	return err
}, handle)

func handle(e errqueue.Event) {
	if mtu, ok := e.MTU(); ok {
		log.Printf("path MTU to %v is %d", e.Dest, mtu)
		return
	}
	log.Print(e) // connection refused (icmp type 3 code 3 from 127.0.0.1) for 127.0.0.1:9
}
```

`Send` returns nil when the datagram was sent. It returns a `*DrainError` when the datagram was sent but reading the queue failed, so don't send it again. The send function must send one datagram per call.

For sockets a library creates itself, such as a DNS client's, `Control` does what `Enable` does as each socket is created. It fits `net.Dialer.Control` and `net.ListenConfig.Control` and leaves TCP sockets alone:

```go
d := &net.Dialer{Control: errqueue.Control}
```

`Enable` sets both `IPV6_RECVERR` and `IP_RECVERR` on IPv6 sockets. A dual-stack socket, which is what `net.ListenUDP("udp", nil)` gives you, queues nothing for its IPv4 traffic without the second one.

On other systems `Enable` and `Drain` return `errors.ErrUnsupported` and `Send` only calls the send function.

## What it changes for Go's resolver

Go's resolver dials a connected UDP socket for each query, and its `Dial` hook can enable errqueue on it (`examples/resolver`). Without `IP_RECVERR`, that socket ignores an ICMP net or host unreachable, so a lookup against a server behind an unreachable route waits out every timeout. `examples/resolver/run.sh` times a `LookupHost` against the router from `netns/setup.sh`, with the resolver's default 5 s timeout and 2 attempts. Measured on Linux 6.17.0-1022-azure (CI, Go 1.26.0) and 7.0.0-38 (Go 1.27.1):

| DNS server | Plain | With errqueue |
|---|---|---|
| Behind an IPv6 unreachable route | 10.0 s | under 1 ms |
| Behind an IPv4 unreachable route | 10.0 s | 5.0 s |
| Closed port | 1–2 ms | 1–2 ms |

The IPv4 case stops at 5 s because the router rate-limits ICMP per source. With errqueue the resolver retries at once, so its four queries (A and AAAA, two attempts each) arrive within milliseconds. On 7.0 the router answered three and dropped the fourth (its counters read `IcmpOutDestUnreachs` 3, `IcmpOutRateLimitHost` 1), and that query waited out its timeout. A closed port already fails fast without errqueue, because a connected socket gets `ECONNREFUSED`.

## What it changes for CoreDNS failover

CoreDNS's `forward` plugin also uses connected UDP sockets, for queries and for the health checks that mark an upstream down. `examples/coredns/coredns.patch` sets `Control` on both dialers, four lines against CoreDNS 1.14.7 (commit `ef18404`). `examples/coredns/run.sh` forwards 50 queries/s to two upstreams in order, makes the first one unreachable 4 s in, and counts the queries slower than 500 ms over 12 s. Measured on 7.0.0-38, the same in three runs for IPv4 and two for IPv6:

| | Unpatched | Patched |
|---|---|---|
| IPv4: slow queries, of about 600 | 301 | 150 |
| IPv6: slow queries, of about 600 | 301 | 116 |

Errqueue only helps the queries whose ICMP error arrives, and the router rate-limits those per source. It sent 3 or 4 IPv4 errors in the whole run and rate-limited the rest, and 45 to 68 IPv6 errors. The rest of the delay is the health checker's: it marks an upstream down after three failed checks 500 ms apart (`plugin/forward/forward.go:34,85` and `plugin/pkg/proxy/proxy.go:146` in CoreDNS).

## What the tests cover

The tests run against real sockets on loopback. On Linux 7.0.0-38-generic x86_64 with Go 1.27.1 they cover port unreachable on IPv4, IPv6 and dual-stack sockets, a local `EMSGSIZE` with its MTU, the read and send side effects above, and full versus partial drains. The parser has unit tests and a fuzz target (`go test -fuzz FuzzParse`).

Loopback can't produce ICMP from a router, so `router_linux_test.go` uses a real one: `netns/setup.sh` puts a Linux router in a network namespace, with a 1300-byte link behind it and unreachable routes. These tests need root and run in CI. They cover time exceeded (IPv4, IPv6, dual-stack), unreachable routes, and "packet too big" from the router followed by the local `EMSGSIZE` the kernel then raises itself, for IPv4 and IPv6. The router quoted 520 payload bytes for IPv4 and 1184 for IPv6.

For a local error on an unconnected IPv4 socket, `Dest` has port 0. Linux reports the socket's connected port there (`net/ipv4/ip_output.c:990` in v7.0), so a connected IPv4 socket gets the right port; IPv6 reports the datagram's. The router tests check all three.

`probes/run.sh` checks the same kernel behaviour from C, without Go in the way.

## Installing

```sh
go get github.com/MohibShaikh/errqueue@latest
```

It needs Go 1.26 or later. BSD-3-Clause licensed; see `LICENSE`.
