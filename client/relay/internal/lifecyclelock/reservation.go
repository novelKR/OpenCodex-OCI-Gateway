package lifecyclelock

import (
	"context"
	"errors"
)

const SourceInstallReservationEnvironment = "OPENCODEX_RELAY_SOURCE_INSTALL_RESERVATION"

const sourceInstallReservationName = ".source-install-reservation.json"

type SourceInstallScope string

const (
	SourceInstallProduction       SourceInstallScope = "production"
	SourceInstallLocalDevelopment SourceInstallScope = "local_development"
)

var (
	ErrReservationBusy   = errors.New("Relay source installation reservation is active")
	ErrReservationUnsafe = errors.New("Relay source installation reservation is unsafe")
)

type SourceInstallReservation struct {
	SchemaVersion int                `json:"schema_version"`
	Scope         SourceInstallScope `json:"scope"`
	Token         string             `json:"token"`
	RootCreated   bool               `json:"root_created"`
}

// ReserveSourceInstall keeps token generation inside the trusted helper and
// durably writes the exact response before publishing the lifecycle marker.
// A supervising installer can therefore recover the token even if its stdout
// capture is interrupted after marker publication.
func ReserveSourceInstall(ctx context.Context, home string, scope SourceInstallScope, recoveryPath string) (SourceInstallReservation, error) {
	return reserveSourceInstall(ctx, home, scope, recoveryPath)
}

func ReleaseSourceInstall(ctx context.Context, home string, scope SourceInstallScope, token string, removeCreatedRoot bool) error {
	return releaseSourceInstall(ctx, home, scope, token, removeCreatedRoot)
}

// ValidateSourceInstallReservation permits an absent reservation or the exact
// token held by the source installer which created it. Every other present or
// malformed marker is a fail-closed writer conflict.
func ValidateSourceInstallReservation(home, token string) error {
	return validateSourceInstallReservation(home, token)
}
