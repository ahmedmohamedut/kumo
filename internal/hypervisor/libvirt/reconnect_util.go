package libvirt

import (
	"errors"
	"io"
	"net"
	"strings"
	"syscall"
)

func shouldReconnect(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) ||
		errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "connection reset") ||
		strings.Contains(s, "EOF")
}

func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}
