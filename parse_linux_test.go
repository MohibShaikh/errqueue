package errqueue

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"reflect"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

func cmsg(level, typ int32, data []byte) []byte {
	b := make([]byte, unix.CmsgSpace(len(data)))
	h := (*unix.Cmsghdr)(unsafe.Pointer(&b[0]))
	h.Level, h.Type = level, typ
	h.SetLen(unix.CmsgLen(len(data)))
	copy(b[unix.CmsgLen(0):], data)
	return b
}

// extErr builds a sock_extended_err followed by an offender sockaddr.
func extErr(errno syscall.Errno, origin Origin, typ, code uint8, info uint32, offender []byte) []byte {
	b := make([]byte, eeLen, eeLen+len(offender))
	binary.NativeEndian.PutUint32(b[0:], uint32(errno))
	b[4], b[5], b[6] = byte(origin), typ, code
	binary.NativeEndian.PutUint32(b[8:], info)
	return append(b, offender...)
}

func sockaddr4(a [4]byte) []byte {
	b := make([]byte, unix.SizeofSockaddrInet4)
	binary.NativeEndian.PutUint16(b, unix.AF_INET)
	copy(b[4:], a[:])
	return b
}

func sockaddr6(a [16]byte, scope uint32) []byte {
	b := make([]byte, unix.SizeofSockaddrInet6)
	binary.NativeEndian.PutUint16(b, unix.AF_INET6)
	copy(b[8:], a[:])
	binary.NativeEndian.PutUint32(b[24:], scope)
	return b
}

func TestParse(t *testing.T) {
	dest := &unix.SockaddrInet4{Addr: [4]byte{192, 0, 2, 1}, Port: 4242}
	mapped := netip.MustParseAddr("::ffff:198.51.100.7").As16()
	linkLocal := netip.MustParseAddr("fe80::1").As16()
	for _, tc := range []struct {
		name    string
		oob     []byte
		flags   int
		from    unix.Sockaddr // dest when nil
		want    Event
		wantErr error
	}{
		{
			name: "ipv4 frag needed",
			oob:  cmsg(unix.SOL_IP, unix.IP_RECVERR, extErr(syscall.EMSGSIZE, OriginICMP, 3, 4, 1400, sockaddr4([4]byte{198, 51, 100, 7}))),
			want: Event{Err: syscall.EMSGSIZE, Origin: OriginICMP, Type: 3, Code: 4, Info: 1400,
				Offender: netip.MustParseAddr("198.51.100.7"), Dest: netip.MustParseAddrPort("192.0.2.1:4242"), Payload: []byte("p")},
		},
		{
			name: "v4-mapped offender is unmapped",
			oob:  cmsg(unix.SOL_IPV6, unix.IPV6_RECVERR, extErr(syscall.EHOSTUNREACH, OriginICMP, 11, 0, 0, sockaddr6(mapped, 0))),
			want: Event{Err: syscall.EHOSTUNREACH, Origin: OriginICMP, Type: 11,
				Offender: netip.MustParseAddr("198.51.100.7"), Dest: netip.MustParseAddrPort("192.0.2.1:4242"), Payload: []byte("p")},
		},
		{
			name:  "local error, truncated payload",
			oob:   cmsg(unix.SOL_IPV6, unix.IPV6_RECVERR, extErr(syscall.EMSGSIZE, OriginLocal, 0, 0, 1280, make([]byte, unix.SizeofSockaddrInet6))),
			flags: unix.MSG_TRUNC,
			want: Event{Err: syscall.EMSGSIZE, Origin: OriginLocal, Info: 1280,
				Dest: netip.MustParseAddrPort("192.0.2.1:4242"), Payload: []byte("p"), Truncated: true},
		},
		{
			name: "other control messages are skipped",
			oob: append(cmsg(unix.SOL_SOCKET, unix.SO_TIMESTAMP, make([]byte, 16)),
				cmsg(unix.SOL_IP, unix.IP_RECVERR, extErr(syscall.ECONNREFUSED, OriginICMP, 3, 3, 0, nil))...),
			want: Event{Err: syscall.ECONNREFUSED, Origin: OriginICMP, Type: 3, Code: 3,
				Dest: netip.MustParseAddrPort("192.0.2.1:4242"), Payload: []byte("p")},
		},
		{
			name: "ipv6 packet too big",
			oob:  cmsg(unix.SOL_IPV6, unix.IPV6_RECVERR, extErr(syscall.EMSGSIZE, OriginICMP6, 2, 0, 1280, sockaddr6(netip.MustParseAddr("2001:db8::7").As16(), 0))),
			from: &unix.SockaddrInet6{Addr: netip.MustParseAddr("2001:db8::1").As16(), Port: 4242},
			want: Event{Err: syscall.EMSGSIZE, Origin: OriginICMP6, Type: 2, Info: 1280,
				Offender: netip.MustParseAddr("2001:db8::7"), Dest: netip.MustParseAddrPort("[2001:db8::1]:4242"), Payload: []byte("p")},
		},
		{
			name: "link-local zones are kept",
			oob:  cmsg(unix.SOL_IPV6, unix.IPV6_RECVERR, extErr(syscall.ECONNREFUSED, OriginICMP6, 1, 4, 0, sockaddr6(linkLocal, 3))),
			from: &unix.SockaddrInet6{Addr: linkLocal, Port: 4242, ZoneId: 7},
			want: Event{Err: syscall.ECONNREFUSED, Origin: OriginICMP6, Type: 1, Code: 4,
				Offender: netip.MustParseAddr("fe80::1%3"), Dest: netip.MustParseAddrPort("[fe80::1%7]:4242"), Payload: []byte("p")},
		},
		{
			name:  "control truncated after a complete error",
			oob:   cmsg(unix.SOL_IPV6, unix.IPV6_RECVERR, extErr(syscall.EHOSTUNREACH, OriginICMP6, 1, 0, 0, nil)),
			flags: unix.MSG_CTRUNC,
			want: Event{Err: syscall.EHOSTUNREACH, Origin: OriginICMP6, Type: 1,
				Dest: netip.MustParseAddrPort("192.0.2.1:4242"), Payload: []byte("p"), ControlTruncated: true},
		},
		{name: "short error", oob: cmsg(unix.SOL_IP, unix.IP_RECVERR, make([]byte, eeLen-1)), wantErr: errShort},
		{name: "no error message", oob: cmsg(unix.SOL_SOCKET, unix.SO_TIMESTAMP, make([]byte, 16)), wantErr: errNoRecvErr},
		{name: "control truncated", flags: unix.MSG_CTRUNC, wantErr: errNoRecvErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			from := tc.from
			if from == nil {
				from = dest
			}
			got, err := parse([]byte("p"), tc.oob, tc.flags, from)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr != nil {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got  %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

func FuzzParse(f *testing.F) {
	f.Add([]byte("p"), cmsg(unix.SOL_IP, unix.IP_RECVERR, extErr(syscall.ECONNREFUSED, OriginICMP, 3, 3, 0, sockaddr4([4]byte{127, 0, 0, 1}))), 0)
	f.Add([]byte{}, cmsg(unix.SOL_IPV6, unix.IPV6_RECVERR, extErr(syscall.EMSGSIZE, OriginLocal, 0, 0, 1280, sockaddr6([16]byte{}, 0))), unix.MSG_TRUNC)
	f.Add([]byte{}, []byte{}, unix.MSG_CTRUNC)
	f.Fuzz(func(t *testing.T, data, oob []byte, flags int) {
		e, err := parse(data, oob, flags, nil)
		if err == nil {
			_ = e.String()
		}
	})
}
