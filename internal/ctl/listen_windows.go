package ctl

import (
	"errors"
	"net"
)

// Listen is not supported on Windows: there are no file modes or peer credentials to restrict the
// socket with, so the command opens the database itself while the service is stopped.
func Listen(string) (net.Listener, error) { return nil, errors.ErrUnsupported }
