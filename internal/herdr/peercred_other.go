//go:build !darwin && !linux

package herdr

import (
	"fmt"
	"net"
	"runtime"
)

// peerUID has no implementation here, so every connection is refused rather
// than trusted unverified.
func peerUID(net.Conn) (uint32, error) {
	return 0, fmt.Errorf("reading a Unix socket peer's uid is not supported on %s", runtime.GOOS)
}
