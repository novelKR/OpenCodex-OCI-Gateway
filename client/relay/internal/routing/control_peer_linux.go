//go:build linux

package routing

import (
	"net"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Getuid())
}

func verifyControlPeer(connection *net.UnixConn) error {
	if connection == nil {
		return ErrControlUnauthorized
	}
	raw, err := connection.SyscallConn()
	if err != nil {
		return ErrControlUnauthorized
	}
	var verified bool
	controlErr := raw.Control(func(fd uintptr) {
		credential, credentialErr := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		verified = credentialErr == nil && credential != nil && credential.Uid == uint32(os.Getuid())
	})
	if controlErr != nil || !verified {
		return ErrControlUnauthorized
	}
	return nil
}
