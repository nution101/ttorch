//go:build darwin

package herdr

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerUID returns the uid of the process on the other end of a Unix socket,
// as the kernel recorded it when the server called listen: LOCAL_PEERCRED.
func peerUID(conn net.Conn) (uint32, error) {
	var uid uint32
	err := controlUnix(conn, func(fd int) error {
		cred, err := unix.GetsockoptXucred(fd, unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if err != nil {
			return err
		}
		uid = cred.Uid
		return nil
	})
	return uid, err
}
