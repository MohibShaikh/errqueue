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

`Enable` sets both `IPV6_RECVERR` and `IP_RECVERR` on IPv6 sockets. A dual-stack socket, which is what `net.ListenUDP("udp", nil)` gives you, queues nothing for its IPv4 traffic without the second one.

On other systems `Enable` and `Drain` return `errors.ErrUnsupported` and `Send` only calls the send function.

## What the tests cover

The tests run against real sockets on loopback. On Linux 7.0.0-38-generic x86_64 with Go 1.27.1 they cover port unreachable on IPv4, IPv6 and dual-stack sockets, a local `EMSGSIZE` with its MTU, the read and send side effects above, and full versus partial drains. The parser has unit tests and a fuzz target (`go test -fuzz FuzzParse`).

Loopback can't produce ICMP from a router, so time exceeded and a remote "packet too big" (IPv4 and IPv6) are only covered by parser tests.

`probes/run.sh` checks the same kernel behaviour from C, without Go in the way.

## Installing from a private repo

The repo is private, so the public module proxy can't fetch it. Set `GOPRIVATE` and make sure git can authenticate to GitHub:

```sh
go env -w GOPRIVATE=github.com/MohibShaikh/*
go get github.com/MohibShaikh/errqueue@v0.1.1
```
