package errqueue

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// Tests against ICMP from a real router, which loopback can't produce. They need
// the namespaces netns/setup.sh builds, so they run only as root with
// ERRQUEUE_NETNS=1 (see .github/workflows/test.yml). The router's MTU report is
// cached per destination, so run them once per fresh setup.

var (
	routerV4 = netip.MustParseAddr("10.0.1.2")
	routerV6 = netip.MustParseAddr("fd00:1::2")
)

func routed(t *testing.T) {
	t.Helper()
	if os.Getenv("ERRQUEUE_NETNS") == "" {
		t.Skip("needs ERRQUEUE_NETNS=1 and the namespaces from netns/setup.sh")
	}
}

// routerEvent sends payload to dst and returns the one error it queues.
func routerEvent(t *testing.T, c *net.UDPConn, dst netip.AddrPort, payload []byte) Event {
	t.Helper()
	if _, err := c.WriteToUDPAddrPort(payload, dst); err != nil {
		t.Fatal(err)
	}
	waitErr(t, c)
	evs := drainAll(t, c)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	t.Logf("%v, %d payload bytes quoted", evs[0], len(evs[0].Payload))
	return evs[0]
}

func TestRouterErrors(t *testing.T) {
	routed(t)
	for _, tc := range []struct {
		name, network, ip, dst string
		hops                   int // 0 keeps the default hop limit
		err                    syscall.Errno
		origin                 Origin
		typ, code              uint8
		offender               netip.Addr
	}{
		{"ipv4 time exceeded", "udp4", "0.0.0.0", "10.0.2.2:9", 1, syscall.EHOSTUNREACH, OriginICMP, 11, 0, routerV4},
		{"ipv6 time exceeded", "udp6", "::", "[fd00:2::2]:9", 1, syscall.EHOSTUNREACH, OriginICMP6, 3, 0, routerV6},
		{"dual-stack time exceeded", "udp", "::", "10.0.2.2:9", 1, syscall.EHOSTUNREACH, OriginICMP, 11, 0, routerV4},
		{"ipv4 unreachable route", "udp4", "0.0.0.0", "10.0.9.1:9", 0, syscall.EHOSTUNREACH, OriginICMP, 3, 1, routerV4},
		{"ipv6 unreachable route", "udp6", "::", "[fd00:9::1]:9", 0, syscall.ENETUNREACH, OriginICMP6, 1, 0, routerV6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := enabled(t, tc.network, tc.ip)
			if tc.hops != 0 {
				control(t, c, func(fd int) error {
					if tc.network == "udp6" {
						return unix.SetsockoptInt(fd, unix.SOL_IPV6, unix.IPV6_UNICAST_HOPS, tc.hops)
					}
					return unix.SetsockoptInt(fd, unix.SOL_IP, unix.IP_TTL, tc.hops)
				})
			}
			dst := netip.MustParseAddrPort(tc.dst)
			e := routerEvent(t, c, dst, []byte("hi"))
			if e.Err != tc.err || e.Origin != tc.origin || e.Type != tc.typ || e.Code != tc.code ||
				e.Offender != tc.offender || e.Dest != dst || string(e.Payload) != "hi" {
				t.Errorf("got %+v", e)
			}
		})
	}
}

// The router can't forward 1400 bytes onto its 1300-byte link and reports the
// MTU. The kernel caches it, so the next oversized send fails locally with the
// same MTU, the IPv4 case loopback can't reach.
func TestRouterPacketTooBig(t *testing.T) {
	routed(t)
	for _, tc := range []struct {
		name, network, ip, dst string
		origin                 Origin
		typ, code              uint8
		offender               netip.Addr
		quoted                 int // payload bytes the router quotes back
	}{
		{"ipv4", "udp4", "0.0.0.0", "10.0.2.2:9", OriginICMP, 3, 4, routerV4, 576 - 20 - 8 - 20 - 8},
		{"ipv6", "udp6", "::", "[fd00:2::2]:9", OriginICMP6, 2, 0, routerV6, 1280 - 40 - 8 - 40 - 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dontFrag := func(fd int) error {
				if tc.network == "udp6" {
					return unix.SetsockoptInt(fd, unix.SOL_IPV6, unix.IPV6_DONTFRAG, 1)
				}
				return unix.SetsockoptInt(fd, unix.SOL_IP, unix.IP_MTU_DISCOVER, unix.IP_PMTUDISC_DO)
			}
			c := enabled(t, tc.network, tc.ip)
			control(t, c, dontFrag)
			dst := netip.MustParseAddrPort(tc.dst)
			e := routerEvent(t, c, dst, make([]byte, 1400))
			mtu, ok := e.MTU()
			if e.Err != syscall.EMSGSIZE || e.Origin != tc.origin || e.Type != tc.typ || e.Code != tc.code ||
				!ok || mtu != 1300 || e.Offender != tc.offender || e.Dest != dst || len(e.Payload) != tc.quoted {
				t.Fatalf("router report: got %+v", e)
			}

			if _, err := c.WriteToUDPAddrPort(make([]byte, 1400), dst); !errors.Is(err, syscall.EMSGSIZE) {
				t.Fatalf("second oversized send: want EMSGSIZE, got %v", err)
			}
			evs := drainAll(t, c)
			if len(evs) != 1 {
				t.Fatalf("after the second send: got %d events, want 1", len(evs))
			}
			t.Log(evs[0])
			want := dst
			if tc.network == "udp4" {
				want = netip.AddrPortFrom(dst.Addr(), 0) // IPv4 reports the connected port, none here
			}
			mtu, ok = evs[0].MTU()
			if evs[0].Origin != OriginLocal || !ok || mtu != 1300 || evs[0].Offender.IsValid() || evs[0].Dest != want {
				t.Errorf("local report: got %+v", evs[0])
			}

			// A connected socket to the same destination gets the cached MTU too, and
			// its local report carries the port on IPv4 as well.
			cc, err := net.DialUDP(tc.network, nil, net.UDPAddrFromAddrPort(dst))
			if err != nil {
				t.Fatal(err)
			}
			defer cc.Close()
			if err := Enable(cc); err != nil {
				t.Fatal(err)
			}
			control(t, cc, dontFrag)
			if _, err := cc.Write(make([]byte, 1400)); !errors.Is(err, syscall.EMSGSIZE) {
				t.Fatalf("connected oversized send: want EMSGSIZE, got %v", err)
			}
			evs = drainAll(t, cc)
			if len(evs) != 1 {
				t.Fatalf("connected: got %d events, want 1", len(evs))
			}
			t.Log(evs[0])
			mtu, ok = evs[0].MTU()
			if evs[0].Origin != OriginLocal || !ok || mtu != 1300 || evs[0].Dest != dst {
				t.Errorf("connected local report: got %+v", evs[0])
			}
		})
	}
}
