package libvirt

import (
	"errors"
	"net"
	"strings"
)

func shouldReconnect(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	errStr := err.Error()
	switch {
	case errors.Is(err, net.ErrClosed):
		return true
	case contains(errStr, "broken pipe"), contains(errStr, "connection reset"), contains(errStr, "EOF"):
		return true
	}
	return false
}

func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}
