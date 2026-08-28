//go:build !darwin

package handoff

import (
	"crypto/sha256"
	"encoding/hex"
)

func currentBootSessionID() (string, bool, error) {
	digest := sha256.Sum256([]byte("pw-open-codex-boot-session-unattested-v1"))
	return hex.EncodeToString(digest[:]), false, nil
}
