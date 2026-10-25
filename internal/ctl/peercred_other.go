//go:build !linux && !darwin && !windows

package ctl

import (
	"errors"
	"net"
)

// Elsewhere the socket's file mode is the only restriction.
func peerUID(net.Conn) (int, error) { return 0, errors.ErrUnsupported }
