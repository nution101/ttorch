//go:build linux

package herdr

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerUID returns the uid of the process on the other end of a Unix socket,
// as the kernel recorded it when the server called listen: SO_PEERCRED.
func peerUID(conn net.Conn) (uint32, error) {
	var uid uint32
	err := controlUnix(conn, func(fd int) error {
		cred, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
		if err != nil {
			return err
		}
		uid = cred.Uid
		return nil
	})
	return uid, err
}
