//go:build linux
// +build linux

package forwardproxy

import (
	"context"
	"os"
	"syscall"
)

func freebindControlContext(ctx context.Context, network, address string, c syscall.RawConn) error {
	var operr error
	if err := c.Control(func(fd uintptr) {
		operr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_FREEBIND, 1)
	}); err != nil {
		return err
	}
	return os.NewSyscallError("setsockopt", operr)
}
