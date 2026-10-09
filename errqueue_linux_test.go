package errqueue

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func listen(t *testing.T, network, ip string) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP(network, &net.UDPAddr{IP: net.ParseIP(ip)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func enabled(t *testing.T, network, ip string) *net.UDPConn {
	t.Helper()
	c := listen(t, network, ip)
	if err := Enable(c); err != nil {
		t.Fatal(err)
	}
	return c
}

func control(t *testing.T, c syscall.Conn, f func(fd int) error) {
	t.Helper()
	rc, err := c.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var ferr error
	if err := rc.Control(func(fd uintptr) { ferr = f(int(fd)) }); err != nil {
		t.Fatal(err)
	}
	if ferr != nil {
		t.Fatal(ferr)
	}
}

// closedAddr returns a loopback port nothing listens on.
func closedAddr(t *testing.T, network, ip string) *net.UDPAddr {
	t.Helper()
	c := listen(t, network, ip)
	a := c.LocalAddr().(*net.UDPAddr)
	c.Close()
	return a
}

// waitErr waits until the error queue is non-empty (POLLERR is always reported).
func waitErr(t *testing.T, c syscall.Conn) {
	t.Helper()
	control(t, c, func(fd int) error {
		p := []unix.PollFd{{Fd: int32(fd)}}
		n, err := unix.Poll(p, 1000)
		if err == nil && (n == 0 || p[0].Revents&unix.POLLERR == 0) {
			err = errors.New("no error queued within 1s")
		}
		return err
	})
}

// queueErrors sends to a closed port until n errors are queued. A send that fails
// with ECONNREFUSED was swallowed by the previous error and is retried.
func queueErrors(t *testing.T, c *net.UDPConn, n int) {
	t.Helper()
	dst := closedAddr(t, "udp4", "127.0.0.1")
	for i := 0; i < n; i++ {
		for {
			_, err := c.WriteToUDP([]byte("x"), dst)
			if err == nil {
				break
			}
			if !errors.Is(err, syscall.ECONNREFUSED) {
				t.Fatal(err)
			}
		}
		waitErr(t, c)
		time.Sleep(20 * time.Millisecond)
	}
}

// readOne reads a single entry, which Drain never does.
func readOne(fd, flags int) (Event, error) {
	data, oob := make([]byte, payloadMax), make([]byte, oobMax)
	n, oobn, rflags, from, err := unix.Recvmsg(fd, data, oob, unix.MSG_ERRQUEUE|flags)
	if err != nil {
		return Event{}, err
	}
	return parse(data[:n], oob[:oobn], rflags, from)
}

func drainAll(t *testing.T, c syscall.Conn) []Event {
	t.Helper()
	var evs []Event
	n, err := Drain(c, func(e Event) { evs = append(evs, e) })
	if err != nil || n != len(evs) {
		t.Fatalf("Drain: n=%d events=%d err=%v", n, len(evs), err)
	}
	return evs
}

func received(t *testing.T, c *net.UDPConn) int {
	t.Helper()
	n := 0
	buf := make([]byte, 64)
	for {
		c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		if _, _, err := c.ReadFromUDP(buf); err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				return n
			}
			t.Fatal(err)
		}
		n++
	}
}

func TestPortUnreachable(t *testing.T) {
	for _, tc := range []struct {
		name, network, ip string
		origin            Origin
		typ, code         uint8
	}{
		{"ipv4", "udp4", "127.0.0.1", OriginICMP, 3, 3},
		{"ipv6", "udp6", "::1", OriginICMP6, 1, 4},
		// A dual-stack socket sending IPv4: queued only because Enable also sets IP_RECVERR.
		{"dual-stack", "udp", "::", OriginICMP, 3, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := enabled(t, tc.network, tc.ip)
			dstNet, dstIP := tc.network, tc.ip
			if tc.name == "dual-stack" {
				dstNet, dstIP = "udp4", "127.0.0.1"
			}
			dst := closedAddr(t, dstNet, dstIP)
			if _, err := c.WriteToUDP([]byte("hi"), dst); err != nil {
				t.Fatal(err)
			}
			waitErr(t, c)
			evs := drainAll(t, c)
			if len(evs) != 1 {
				t.Fatalf("got %d events, want 1", len(evs))
			}
			e := evs[0]
			t.Log(e)
			want := dst.AddrPort()
			want = netip.AddrPortFrom(want.Addr().Unmap(), want.Port())
			if e.Err != syscall.ECONNREFUSED || e.Origin != tc.origin || e.Type != tc.typ || e.Code != tc.code ||
				e.Offender != want.Addr() || e.Dest != want || string(e.Payload) != "hi" || e.Truncated {
				t.Errorf("got %+v", e)
			}
		})
	}
}

// Without IP_RECVERR a dual-stack socket queues nothing for IPv4 traffic, which is
// why Enable sets it on IPv6 sockets too.
// Control enables the queue on the UDP sockets a Dialer creates and leaves TCP
// sockets alone.
func TestControl(t *testing.T) {
	d := net.Dialer{Control: Control}
	c, err := d.Dial("udp4", closedAddr(t, "udp4", "127.0.0.1").String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	waitErr(t, c.(*net.UDPConn))
	if evs := drainAll(t, c.(*net.UDPConn)); len(evs) != 1 || evs[0].Err != syscall.ECONNREFUSED {
		t.Fatalf("got %v, want one ECONNREFUSED", evs)
	}

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	tc, err := d.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	control(t, tc.(*net.TCPConn), func(fd int) error {
		if v, err := unix.GetsockoptInt(fd, unix.SOL_IP, unix.IP_RECVERR); err != nil || v != 0 {
			return errors.Join(err, errors.New("IP_RECVERR is set on a TCP socket"))
		}
		return nil
	})
}

func TestDualStackNeedsIPRecvErr(t *testing.T) {
	c := listen(t, "udp", "::")
	control(t, c, func(fd int) error { return unix.SetsockoptInt(fd, unix.SOL_IPV6, unix.IPV6_RECVERR, 1) })
	if _, err := c.WriteToUDP([]byte("hi"), closedAddr(t, "udp4", "127.0.0.1")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if n, err := Drain(c, nil); n != 0 || err != nil {
		t.Fatalf("IPV6_RECVERR alone: drained %d, %v; want 0", n, err)
	}
}

// A send larger than the socket's path MTU fails with EMSGSIZE and queues a local
// entry carrying the MTU. Send must not retry it.
func TestLocalPacketTooBig(t *testing.T) {
	c, err := net.DialUDP("udp6", nil, &net.UDPAddr{IP: net.IPv6loopback, Port: 9})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := Enable(c); err != nil {
		t.Fatal(err)
	}
	control(t, c, func(fd int) error {
		return errors.Join(unix.SetsockoptInt(fd, unix.SOL_IPV6, unix.IPV6_DONTFRAG, 1),
			unix.SetsockoptInt(fd, unix.SOL_IPV6, unix.IPV6_MTU, 1280))
	})
	sends := 0
	var evs []Event
	err = Send(c, func() error { sends++; _, err := c.Write(make([]byte, 1400)); return err },
		func(e Event) { evs = append(evs, e) })
	if !errors.Is(err, syscall.EMSGSIZE) || sends != 1 || len(evs) != 1 {
		t.Fatalf("Send: err=%v sends=%d events=%d; want EMSGSIZE, 1, 1", err, sends, len(evs))
	}
	e := evs[0]
	t.Log(e)
	mtu, ok := e.MTU()
	if e.Origin != OriginLocal || !ok || mtu != 1280 || e.Offender.IsValid() {
		t.Errorf("got %+v", e)
	}
}

// A goroutine parked in ReadFromUDP is woken by an ICMP error and gets ECONNREFUSED,
// although no datagram arrived. Without IP_RECVERR it keeps waiting.
func TestBlockedReadReturnsQueuedError(t *testing.T) {
	for _, enable := range []bool{true, false} {
		c := listen(t, "udp4", "127.0.0.1")
		if enable {
			if err := Enable(c); err != nil {
				t.Fatal(err)
			}
		}
		c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		done := make(chan error, 1)
		go func() {
			_, _, err := c.ReadFromUDP(make([]byte, 64))
			done <- err
		}()
		time.Sleep(50 * time.Millisecond) // let the reader park
		if _, err := c.WriteToUDP([]byte("x"), closedAddr(t, "udp4", "127.0.0.1")); err != nil {
			t.Fatal(err)
		}
		err := <-done
		t.Logf("enabled=%v: ReadFromUDP returned %v", enable, err)
		if enable && !errors.Is(err, syscall.ECONNREFUSED) {
			t.Errorf("enabled: want ECONNREFUSED, got %v", err)
		}
		if !enable && !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("not enabled: want deadline exceeded, got %v", err)
		}
	}
}

// After that spurious read error the socket still works: data is received, and with
// the queue left undrained the next read waits for its deadline instead of failing.
func TestReadWorksAfterQueuedError(t *testing.T) {
	c, peer := enabled(t, "udp4", "127.0.0.1"), listen(t, "udp4", "127.0.0.1")
	queueErrors(t, c, 1)
	c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	_, _, err := c.ReadFromUDP(make([]byte, 64))
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("first read: want ECONNREFUSED, got %v", err)
	}
	if _, err := peer.WriteToUDP([]byte("data"), c.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, _, err := c.ReadFromUDP(buf)
	if err != nil || string(buf[:n]) != "data" {
		t.Fatalf("second read: got %q, %v", buf[:n], err)
	}
	c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, _, err = c.ReadFromUDP(buf); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("third read: want deadline exceeded, got %v", err)
	}
	if evs := drainAll(t, c); len(evs) != 1 {
		t.Fatalf("entry should still be queued: drained %d", len(evs))
	}
}

// Quinn #2704 through net.UDPConn: a queued error fails the next WriteToUDP to a live
// peer and the datagram is not sent. Three queued errors still fail only one write.
// Send absorbs it.
func TestQueuedErrorFailsNextWrite(t *testing.T) {
	for _, queued := range []int{1, 3} {
		c, peer := enabled(t, "udp4", "127.0.0.1"), listen(t, "udp4", "127.0.0.1")
		queueErrors(t, c, queued)
		dst := peer.LocalAddr().(*net.UDPAddr)
		fails := 0
		for {
			_, err := c.WriteToUDP([]byte("ok"), dst)
			if err == nil {
				break
			}
			if !errors.Is(err, syscall.ECONNREFUSED) || fails == 5 {
				t.Fatal(err)
			}
			fails++
		}
		if got := received(t, peer); fails != 1 || got != 1 {
			t.Errorf("%d queued: %d failed writes, %d delivered; want 1 and 1", queued, fails, got)
		}
	}
}

func TestSendRetriesAfterQueuedError(t *testing.T) {
	c, peer := enabled(t, "udp4", "127.0.0.1"), listen(t, "udp4", "127.0.0.1")
	queueErrors(t, c, 3)
	dst := peer.LocalAddr().(*net.UDPAddr)
	sends := 0
	var evs []Event
	err := Send(c, func() error { sends++; _, err := c.WriteToUDP([]byte("ok"), dst); return err },
		func(e Event) { evs = append(evs, e) })
	if err != nil || sends != 2 || len(evs) != 3 || received(t, peer) != 1 {
		t.Fatalf("err=%v sends=%d events=%d; want nil, 2, 3 and one delivery", err, sends, len(evs))
	}
	if err := Send(c, func() error { _, err := c.WriteToUDP([]byte("ok"), dst); return err }, nil); err != nil {
		t.Fatalf("Send with an empty queue: %v", err)
	}
}

// Reading the queue until EAGAIN clears the pending error; reading part of it re-arms it.
func TestDrainClearsPendingError(t *testing.T) {
	for _, full := range []bool{true, false} {
		c, peer := enabled(t, "udp4", "127.0.0.1"), listen(t, "udp4", "127.0.0.1")
		queueErrors(t, c, 3)
		if full {
			if evs := drainAll(t, c); len(evs) != 3 {
				t.Fatalf("drained %d entries, want 3", len(evs))
			}
		} else {
			control(t, c, func(fd int) error { _, err := readOne(fd, unix.MSG_DONTWAIT); return err })
		}
		_, err := c.WriteToUDP([]byte("ok"), peer.LocalAddr().(*net.UDPAddr))
		t.Logf("full drain=%v: next write returned %v", full, err)
		if full && err != nil {
			t.Errorf("after full drain: want nil, got %v", err)
		}
		if !full && !errors.Is(err, syscall.ECONNREFUSED) {
			t.Errorf("after partial drain: want ECONNREFUSED, got %v", err)
		}
	}
}

// RawConn.Read parks on the netpoller until an error is queued, so a reader can wait
// for the error queue without a busy loop or a second fd.
func TestRawConnReadWaitsForErrQueue(t *testing.T) {
	c := enabled(t, "udp4", "127.0.0.1")
	rc, err := c.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	type result struct {
		ev      Event
		err     error
		attempt int
	}
	done := make(chan result, 1)
	go func() {
		var r result
		err := rc.Read(func(fd uintptr) bool {
			r.attempt++
			r.ev, r.err = readOne(int(fd), unix.MSG_DONTWAIT)
			return !errors.Is(r.err, unix.EAGAIN)
		})
		if err != nil {
			r.err = err
		}
		done <- r
	}()
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	if _, err := c.WriteToUDP([]byte("x"), closedAddr(t, "udp4", "127.0.0.1")); err != nil {
		t.Fatal(err)
	}
	r := <-done
	t.Logf("woke after %v, %d attempts, err=%v", time.Since(start), r.attempt, r.err)
	if r.err != nil || r.ev.Origin != OriginICMP || r.ev.Err != syscall.ECONNREFUSED {
		t.Fatalf("got %+v, %v", r.ev, r.err)
	}
	if r.attempt != 2 {
		t.Errorf("want 2 attempts (EAGAIN, then the entry), got %d", r.attempt)
	}
}

// Regressions found in the v0.1.0 audit.

// A callback that closes the socket used to wait forever: callbacks ran inside
// RawConn.Control, which holds the reference Close waits for.
func TestDrainCallbackCanClose(t *testing.T) {
	c := enabled(t, "udp4", "127.0.0.1")
	queueErrors(t, c, 1)
	done := make(chan error, 1)
	go func() {
		_, err := Drain(c, func(Event) { done <- c.Close() })
		if err != nil {
			done <- err
		}
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close inside the Drain callback did not return")
	}
}

// An earlier oversized send leaves a local entry queued. It used to stop Send
// retrying a later send that an ICMP error failed, so that datagram was lost.
func TestSendRetriesPastStaleLocalEntry(t *testing.T) {
	peer := listen(t, "udp6", "::1")
	addr := peer.LocalAddr().(*net.UDPAddr)
	c, err := net.DialUDP("udp6", nil, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := Enable(c); err != nil {
		t.Fatal(err)
	}
	control(t, c, func(fd int) error {
		return errors.Join(unix.SetsockoptInt(fd, unix.SOL_IPV6, unix.IPV6_DONTFRAG, 1),
			unix.SetsockoptInt(fd, unix.SOL_IPV6, unix.IPV6_MTU, 1280))
	})
	if _, err := c.Write(make([]byte, 1400)); !errors.Is(err, syscall.EMSGSIZE) {
		t.Fatalf("oversized write: %v", err)
	}
	peer.Close()
	if _, err := c.Write([]byte("x")); err != nil { // to the closed port: queues an ICMP6 error
		t.Fatal(err)
	}
	waitErr(t, c)
	time.Sleep(30 * time.Millisecond)
	alive, err := net.ListenUDP("udp6", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer alive.Close()
	var origins []Origin
	err = Send(c, func() error { _, err := c.Write([]byte("ok")); return err },
		func(e Event) { origins = append(origins, e.Origin) })
	if err != nil || received(t, alive) != 1 {
		t.Fatalf("Send: %v, origins %v; want the datagram delivered", err, origins)
	}
}

// A send error ICMP cannot cause is returned untouched and leaves the queue alone.
func TestSendIgnoresNonICMPErrors(t *testing.T) {
	c := enabled(t, "udp4", "127.0.0.1")
	queueErrors(t, c, 1)
	calls := 0
	err := Send(c, func() error { calls++; return syscall.EINVAL }, nil)
	if !errors.Is(err, syscall.EINVAL) || calls != 1 {
		t.Fatalf("err=%v calls=%d; want EINVAL after one call", err, calls)
	}
	if n, _ := Drain(c, nil); n != 1 {
		t.Fatalf("queue: %d entries, want the 1 Send left alone", n)
	}
}

// Another goroutine drains the queue between the failed send and Send's own
// drain. Send used to give up on an empty drain, losing the datagram.
func TestSendRetriesWhenQueueDrainedElsewhere(t *testing.T) {
	c, peer := enabled(t, "udp4", "127.0.0.1"), listen(t, "udp4", "127.0.0.1")
	queueErrors(t, c, 3)
	calls := 0
	err := Send(c, func() error {
		calls++
		_, err := c.WriteToUDP([]byte("ok"), peer.LocalAddr().(*net.UDPAddr))
		if calls == 1 {
			if n, derr := Drain(c, nil); n != 3 || derr != nil {
				t.Errorf("competing Drain: %d, %v", n, derr)
			}
		}
		return err
	}, nil)
	if err != nil || calls != 2 || received(t, peer) != 1 {
		t.Fatalf("err=%v calls=%d; want the retry delivered", err, calls)
	}
}

type failingControl struct{ syscall.Conn }
type failingRawConn struct{ syscall.RawConn }

var errControl = errors.New("injected Control failure")

func (c failingControl) SyscallConn() (syscall.RawConn, error) {
	rc, err := c.Conn.SyscallConn()
	return failingRawConn{rc}, err
}

func (r failingRawConn) Control(f func(uintptr)) error {
	return errors.Join(r.RawConn.Control(f), errControl)
}

// A retry that succeeds used to drop the drain's error. It now comes back as a
// *DrainError, which says the datagram was sent.
func TestSendReportsDrainError(t *testing.T) {
	c := enabled(t, "udp4", "127.0.0.1")
	queueErrors(t, c, 1)
	calls := 0
	err := Send(failingControl{c}, func() error {
		calls++
		if calls == 1 {
			return syscall.ECONNREFUSED
		}
		return nil
	}, nil)
	var de *DrainError
	if !errors.As(err, &de) || !errors.Is(err, errControl) || calls != 2 {
		t.Fatalf("err=%v calls=%d; want a *DrainError after the retry", err, calls)
	}
}
