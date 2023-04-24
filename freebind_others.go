//go:build !linux
// +build !linux

package forwardproxy

import (
	"context"
	"syscall"
)

func freebindControlContext(ctx context.Context, network, address string, c syscall.RawConn) error {
	return nil
}
