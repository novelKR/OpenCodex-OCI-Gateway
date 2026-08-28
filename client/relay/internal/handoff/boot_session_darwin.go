//go:build darwin

package handoff

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"golang.org/x/sys/unix"
)

func currentBootSessionID() (string, bool, error) {
	value, err := unix.Sysctl("kern.bootsessionuuid")
	if err != nil {
		return "", false, err
	}
	value = strings.TrimSpace(value)
	if len(value) != 36 {
		return "", false, ErrRemovalCleanupUnsafe
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return "", false, ErrRemovalCleanupUnsafe
			}
			continue
		}
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') || (character >= 'A' && character <= 'F')) {
			return "", false, ErrRemovalCleanupUnsafe
		}
	}
	digest := sha256.Sum256([]byte("pw-open-codex-boot-session-v1\x00" + strings.ToLower(value)))
	return hex.EncodeToString(digest[:]), true, nil
}
