//go:build !darwin && !linux

package routing

import (
	"net"
	"os"
)

func ownedByCurrentUser(os.FileInfo) bool { return false }

func verifyControlPeer(*net.UnixConn) error { return ErrControlUnauthorized }
