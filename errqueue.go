// Package errqueue reads ICMP errors and local send errors from a UDP socket's
// error queue on Linux (IP_RECVERR, IPV6_RECVERR, MSG_ERRQUEUE).
//
// Enabling the queue changes how the socket behaves: the kernel also reports each
// new error as the result of the next ordinary read or send. A blocked ReadFrom
// returns ECONNREFUSED or EHOSTUNREACH without data, and a send fails without
// sending its datagram, whatever its destination. Call Drain when a read fails,
// and send through Send, so these reports are consumed instead of treated as
// failures. Give each socket one goroutine that drains it.
//
// On other systems Enable and Drain return errors.ErrUnsupported and Send only
// calls the send function.
package errqueue

import (
	"fmt"
	"net/netip"
	"syscall"
)

// Origin says where an error was raised (ee_origin).
type Origin uint8

const (
	OriginNone  Origin = 0
	OriginLocal Origin = 1 // the local stack, e.g. EMSGSIZE against a known path MTU
	OriginICMP  Origin = 2
	OriginICMP6 Origin = 3
)

func (o Origin) String() string {
	switch o {
	case OriginNone:
		return "none"
	case OriginLocal:
		return "local"
	case OriginICMP:
		return "icmp"
	case OriginICMP6:
		return "icmp6"
	}
	return fmt.Sprintf("origin(%d)", uint8(o))
}

// Event is one error queue entry: an ICMP error some host or router sent about a
// datagram this socket sent, or an error the local stack raised for one.
type Event struct {
	Err      syscall.Errno // ECONNREFUSED, EHOSTUNREACH, EMSGSIZE, ...
	Origin   Origin        // where the error was raised
	Type     uint8         // ICMP or ICMPv6 type; 0 for a local error
	Code     uint8         // ICMP or ICMPv6 code; 0 for a local error
	Info     uint32        // the reported MTU when Err is EMSGSIZE
	Offender netip.Addr    // who reported the error; invalid for a local error

	// Dest is where the original datagram was going. Its port is 0 for a local
	// error on an unconnected IPv4 socket: Linux reports the socket's connected
	// port there, not the datagram's.
	Dest netip.AddrPort

	Payload   []byte // the start of the original datagram's payload
	Truncated bool   // the kernel quoted more payload than was read

	// ControlTruncated reports that the kernel had more control data than fit,
	// so Offender may be missing.
	ControlTruncated bool
}

// A DrainError is returned by Send when the datagram was sent but reading the
// error queue failed. The datagram must not be sent again.
type DrainError struct{ Err error }

func (e *DrainError) Error() string {
	return "errqueue: datagram sent, but reading the error queue failed: " + e.Err.Error()
}

func (e *DrainError) Unwrap() error { return e.Err }

// MTU returns the path MTU carried by a "packet too big" error: ICMP
// fragmentation needed, ICMPv6 packet too big, or a local EMSGSIZE.
func (e Event) MTU() (uint32, bool) {
	return e.Info, e.Err == syscall.EMSGSIZE && e.Info != 0
}

func (e Event) String() string {
	s := fmt.Sprintf("%v (%v", e.Err, e.Origin)
	if e.Origin != OriginLocal {
		s += fmt.Sprintf(" type %d code %d", e.Type, e.Code)
	}
	if e.Offender.IsValid() {
		s += " from " + e.Offender.String()
	}
	return s + ") for " + e.Dest.String()
}
