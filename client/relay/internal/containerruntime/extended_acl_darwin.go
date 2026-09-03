//go:build darwin

package containerruntime

import (
	"encoding/binary"
	"errors"
	"unsafe"

	"golang.org/x/sys/unix"
)

const maximumExtendedSecurityBytes = 64 << 10

// hasExtendedACL mirrors the repository's existing macOS trust-boundary
// check. FSOPT_NOFOLLOW is required because pathname metadata validation must
// never turn a symlink into authority over its target.
func hasExtendedACL(path string) (bool, error) {
	pathPointer, err := unix.BytePtrFromString(path)
	if err != nil {
		return false, err
	}
	attributes := unix.Attrlist{
		Bitmapcount: unix.ATTR_BIT_MAP_COUNT,
		Commonattr:  unix.ATTR_CMN_EXTENDED_SECURITY,
	}
	buffer := make([]byte, maximumExtendedSecurityBytes)
	_, _, callErr := unix.Syscall6(
		unix.SYS_GETATTRLIST,
		uintptr(unsafe.Pointer(pathPointer)),
		uintptr(unsafe.Pointer(&attributes)),
		uintptr(unsafe.Pointer(&buffer[0])),
		uintptr(len(buffer)),
		uintptr(unix.FSOPT_NOFOLLOW),
		0,
	)
	if callErr != 0 {
		return false, callErr
	}
	resultLength := binary.LittleEndian.Uint32(buffer[0:4])
	attributeLength := binary.LittleEndian.Uint32(buffer[8:12])
	if resultLength < 12 || resultLength > uint32(len(buffer)) || attributeLength > resultLength-12 {
		return false, errors.New("extended security result is invalid")
	}
	return attributeLength > 0, nil
}
