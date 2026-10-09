//go:build !linux

package errqueue

import (
	"errors"
	"syscall"
)

func Enable(conn syscall.Conn) error { return errors.ErrUnsupported }

func Control(network, address string, c syscall.RawConn) error { return nil }

func Drain(conn syscall.Conn, fn func(Event)) (int, error) { return 0, errors.ErrUnsupported }

func Send(conn syscall.Conn, send func() error, fn func(Event)) error { return send() }
