package errqueue

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	payloadMax = 2048 // longer quotes are cut and reported as Truncated
	oobMax     = 512  // room for the error and any other control messages enabled on the socket
	eeLen      = 16   // struct sock_extended_err
)

var (
	errNoRecvErr = errors.New("errqueue: entry has no IP_RECVERR control message")
	errShort     = errors.New("errqueue: short sock_extended_err")
)

// icmpErrnos are the errnos Linux turns ICMP and ICMPv6 errors into
// (net/ipv4/icmp.c icmp_err_convert, net/ipv4/udp.c __udp4_lib_err,
// net/ipv6/icmp.c icmpv6_err_convert). Only these can reach a send as the
// pending error of an earlier datagram.
var icmpErrnos = map[syscall.Errno]bool{
	syscall.ENETUNREACH:  true,
	syscall.EHOSTUNREACH: true,
	syscall.ENOPROTOOPT:  true,
	syscall.ECONNREFUSED: true,
	syscall.EMSGSIZE:     true,
	syscall.EOPNOTSUPP:   true,
	syscall.EHOSTDOWN:    true,
	syscall.ENONET:       true,
	syscall.EPROTO:       true,
	syscall.EACCES:       true,
}

// Enable turns on the error queue for conn. An IPv6 socket gets both IPV6_RECVERR
// and IP_RECVERR: a dual-stack socket queues errors for its IPv4 traffic only
// with the latter.
func Enable(conn syscall.Conn) error {
	rc, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	err = rc.Control(func(fd uintptr) {
		var domain int
		domain, serr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_DOMAIN)
		switch {
		case serr != nil:
		case domain == unix.AF_INET:
			serr = unix.SetsockoptInt(int(fd), unix.SOL_IP, unix.IP_RECVERR, 1)
		case domain == unix.AF_INET6:
			serr = errors.Join(
				unix.SetsockoptInt(int(fd), unix.SOL_IPV6, unix.IPV6_RECVERR, 1),
				unix.SetsockoptInt(int(fd), unix.SOL_IP, unix.IP_RECVERR, 1))
		default:
			serr = fmt.Errorf("errqueue: unsupported address family %d", domain)
		}
	})
	if err != nil {
		return err
	}
	return serr
}

// Drain reads the error queue until it is empty, without blocking, then calls fn
// for each entry. It always reads to the end: reading part of the queue re-arms
// the socket's pending error, which then fails the next send or read.
//
// n counts the entries read. An entry that cannot be parsed is skipped, and its
// error is returned.
//
// Drain consumes every entry, including SO_TIMESTAMPING and zero-copy
// notifications, which Event does not fully represent. Give each socket one
// owner that drains it: while one goroutine drains, another goroutine's read or
// send can take the re-armed pending error and then find the queue empty.
func Drain(conn syscall.Conn, fn func(Event)) (n int, err error) {
	rc, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	data, oob := make([]byte, payloadMax), make([]byte, oobMax)
	var evs []Event
	var errs []error
	err = rc.Control(func(fd uintptr) {
		for {
			dn, oobn, flags, from, rerr := unix.Recvmsg(int(fd), data, oob, unix.MSG_ERRQUEUE|unix.MSG_DONTWAIT)
			switch {
			case rerr == unix.EINTR:
				continue
			case rerr == unix.EAGAIN:
				return
			case rerr != nil:
				errs = append(errs, rerr)
				return
			}
			n++
			ev, perr := parse(data[:dn], oob[:oobn], flags, from)
			if perr != nil {
				errs = append(errs, perr)
				continue
			}
			evs = append(evs, ev)
		}
	})
	// Callbacks run after Control returns: Control holds a reference on the
	// descriptor, and a callback that closes conn would wait for it forever.
	if fn != nil {
		for _, ev := range evs {
			fn(ev)
		}
	}
	return n, errors.Join(append(errs, err)...)
}

// Send calls send and, if it failed with an error an ICMP message could have
// caused, drains the queue and calls send once more. With the queue enabled the
// kernel fails the first send after an ICMP error arrives, whatever its
// destination, and does not send that datagram. Entries drained on the way are
// passed to fn.
//
// send must send at most one datagram per call. An EMSGSIZE is retried only when
// the queue held an ICMP packet-too-big; otherwise it is this send's own refusal.
//
// A nil error means the datagram was sent. A *DrainError means it was sent but
// reading the queue failed; do not send it again.
func Send(conn syscall.Conn, send func() error, fn func(Event)) error {
	err := send()
	var errno syscall.Errno
	if err == nil || !errors.As(err, &errno) || !icmpErrnos[errno] {
		return err
	}
	retry := errno != syscall.EMSGSIZE
	_, derr := Drain(conn, func(e Event) {
		if e.Err == syscall.EMSGSIZE && e.Origin != OriginLocal {
			retry = true
		}
		if fn != nil {
			fn(e)
		}
	})
	if !retry {
		return errors.Join(err, derr)
	}
	if err := send(); err != nil {
		return errors.Join(err, derr)
	}
	if derr != nil {
		return &DrainError{Err: derr}
	}
	return nil
}

func parse(data, oob []byte, flags int, from unix.Sockaddr) (Event, error) {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return Event{}, fmt.Errorf("errqueue: %w", err)
	}
	for _, m := range msgs {
		if !(m.Header.Level == unix.SOL_IP && m.Header.Type == unix.IP_RECVERR ||
			m.Header.Level == unix.SOL_IPV6 && m.Header.Type == unix.IPV6_RECVERR) {
			continue
		}
		if len(m.Data) < eeLen {
			return Event{}, errShort
		}
		return Event{
			Err:              syscall.Errno(binary.NativeEndian.Uint32(m.Data[0:4])),
			Origin:           Origin(m.Data[4]),
			Type:             m.Data[5],
			Code:             m.Data[6],
			Info:             binary.NativeEndian.Uint32(m.Data[8:12]),
			Offender:         offender(m.Data[eeLen:]),
			Dest:             addrPort(from),
			Payload:          bytes.Clone(data),
			Truncated:        flags&unix.MSG_TRUNC != 0,
			ControlTruncated: flags&unix.MSG_CTRUNC != 0,
		}, nil
	}
	if flags&unix.MSG_CTRUNC != 0 {
		return Event{}, fmt.Errorf("%w (control data truncated)", errNoRecvErr)
	}
	return Event{}, errNoRecvErr
}

// offender reads the sockaddr the kernel places after sock_extended_err
// (SO_EE_OFFENDER). A local error carries AF_UNSPEC.
func offender(b []byte) netip.Addr {
	if len(b) < 2 {
		return netip.Addr{}
	}
	switch binary.NativeEndian.Uint16(b) {
	case unix.AF_INET:
		if len(b) >= 8 {
			return netip.AddrFrom4([4]byte(b[4:8]))
		}
	case unix.AF_INET6:
		if len(b) >= 24 {
			a := netip.AddrFrom16([16]byte(b[8:24])).Unmap()
			if len(b) >= unix.SizeofSockaddrInet6 {
				a = withZone(a, binary.NativeEndian.Uint32(b[24:28]))
			}
			return a
		}
	}
	return netip.Addr{}
}

func addrPort(sa unix.Sockaddr) netip.AddrPort {
	switch sa := sa.(type) {
	case *unix.SockaddrInet4:
		return netip.AddrPortFrom(netip.AddrFrom4(sa.Addr), uint16(sa.Port))
	case *unix.SockaddrInet6:
		return netip.AddrPortFrom(withZone(netip.AddrFrom16(sa.Addr).Unmap(), sa.ZoneId), uint16(sa.Port))
	}
	return netip.AddrPort{}
}

// withZone keeps an IPv6 scope ID as a numeric zone, which net.UDPAddr accepts.
func withZone(a netip.Addr, scope uint32) netip.Addr {
	if scope == 0 || !a.Is6() {
		return a
	}
	return a.WithZone(strconv.FormatUint(uint64(scope), 10))
}
