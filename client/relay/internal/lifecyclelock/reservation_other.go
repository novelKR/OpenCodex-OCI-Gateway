//go:build !darwin

package lifecyclelock

import "context"

func reserveSourceInstall(context.Context, string, SourceInstallScope, string) (SourceInstallReservation, error) {
	return SourceInstallReservation{}, ErrReservationUnsafe
}

func releaseSourceInstall(context.Context, string, SourceInstallScope, string, bool) error {
	return ErrReservationUnsafe
}

func validateSourceInstallReservation(string, string) error { return nil }
