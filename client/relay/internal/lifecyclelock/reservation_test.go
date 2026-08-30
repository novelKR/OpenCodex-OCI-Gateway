//go:build darwin

package lifecyclelock

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSourceInstallReservationSerializesScopesAndAuthorizesOwner(t *testing.T) {
	home := reservationTestHome(t)
	recoveryPath := reservationRecoveryPath(t)
	reservation, err := ReserveSourceInstall(context.Background(), home, SourceInstallProduction, recoveryPath)
	if err != nil {
		t.Fatal(err)
	}
	if reservation.SchemaVersion != 1 || !reservation.RootCreated || !validReservationToken(reservation.Token) {
		t.Fatalf("invalid reservation: %#v", reservation)
	}
	recoveryPayload, err := os.ReadFile(recoveryPath)
	if err != nil {
		t.Fatal(err)
	}
	var recovered SourceInstallReservation
	if err := json.Unmarshal(recoveryPayload, &recovered); err != nil || recovered != reservation {
		t.Fatalf("invalid durable recovery receipt: %#v, err=%v", recovered, err)
	}
	if err := ValidateSourceInstallReservation(home, reservation.Token); err != nil {
		t.Fatalf("owner token rejected: %v", err)
	}
	if err := ValidateSourceInstallReservation(home, ""); !errors.Is(err, ErrReservationBusy) {
		t.Fatalf("foreign writer error = %v", err)
	}
	if _, err := AcquireWriter(context.Background(), home, ""); !errors.Is(err, ErrReservationBusy) {
		t.Fatalf("foreign lifecycle writer error = %v", err)
	}
	writer, err := AcquireWriter(context.Background(), home, reservation.Token)
	if err != nil {
		t.Fatalf("reservation owner lifecycle writer rejected: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ReserveSourceInstall(context.Background(), home, SourceInstallLocalDevelopment, reservationRecoveryPath(t)); !errors.Is(err, ErrReservationBusy) {
		t.Fatalf("second scope reserve error = %v", err)
	}
	if err := ReleaseSourceInstall(
		context.Background(), home, SourceInstallProduction, reservation.Token, true,
	); err != nil {
		t.Fatal(err)
	}
	root, _ := sourceInstallRoot(home, SourceInstallProduction)
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created empty root survived release: %v", err)
	}
}

func TestSourceInstallReservationRejectsIntermediateSymlinkWithoutExternalMutation(t *testing.T) {
	home := reservationTestHome(t)
	external := t.TempDir()
	if err := os.Symlink(external, filepath.Join(home, ".local")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReserveSourceInstall(context.Background(), home, SourceInstallProduction, reservationRecoveryPath(t)); !errors.Is(err, ErrReservationUnsafe) {
		t.Fatalf("reserve error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(external, "lib")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reservation traversed external symlink: %v", err)
	}
}

func TestSourceInstallReservationRejectsScopeMismatchAndDuplicateToken(t *testing.T) {
	for _, test := range []struct {
		name       string
		records    map[SourceInstallScope]sourceInstallReservationRecord
		expected   error
		ownerToken string
	}{
		{
			name: "scope_mismatch",
			records: map[SourceInstallScope]sourceInstallReservationRecord{
				SourceInstallProduction: {SchemaVersion: 1, Scope: SourceInstallLocalDevelopment, Token: stringOfHex('a')},
			},
			expected: ErrReservationUnsafe,
		},
		{
			name: "duplicate_owner_token",
			records: map[SourceInstallScope]sourceInstallReservationRecord{
				SourceInstallProduction:       {SchemaVersion: 1, Scope: SourceInstallProduction, Token: stringOfHex('b')},
				SourceInstallLocalDevelopment: {SchemaVersion: 1, Scope: SourceInstallLocalDevelopment, Token: stringOfHex('b')},
			},
			expected:   ErrReservationUnsafe,
			ownerToken: stringOfHex('b'),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := reservationTestHome(t)
			for scope, record := range test.records {
				root, err := sourceInstallRoot(home, scope)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(root, 0o700); err != nil {
					t.Fatal(err)
				}
				payload, err := json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, sourceInstallReservationName), append(payload, '\n'), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := ValidateSourceInstallReservation(home, test.ownerToken); !errors.Is(err, test.expected) {
				t.Fatalf("validation error = %v, want %v", err, test.expected)
			}
		})
	}
}

func stringOfHex(value byte) string {
	buffer := make([]byte, 64)
	for index := range buffer {
		buffer[index] = value
	}
	return string(buffer)
}

func TestSourceInstallReservationRefusesAnyStandaloneJournal(t *testing.T) {
	for _, name := range []string{"standalone-native", "standalone-native.open-codex-removal.json"} {
		t.Run(name, func(t *testing.T) {
			home := reservationTestHome(t)
			directory := filepath.Join(home, "Library", "Application Support", directoryName)
			if err := os.Mkdir(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, name), []byte("malformed\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReserveSourceInstall(context.Background(), home, SourceInstallProduction, reservationRecoveryPath(t)); !errors.Is(err, ErrReservationBusy) {
				t.Fatalf("reserve error = %v", err)
			}
		})
	}
}

func TestSourceInstallReservationRejectsMalformedMarker(t *testing.T) {
	home := reservationTestHome(t)
	root, _ := sourceInstallRoot(home, SourceInstallProduction)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, sourceInstallReservationName), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSourceInstallReservation(home, ""); !errors.Is(err, ErrReservationUnsafe) {
		t.Fatalf("validation error = %v", err)
	}
}

func TestReleaseCreatedNonemptyRootRetainsMarker(t *testing.T) {
	home := reservationTestHome(t)
	reservation, err := ReserveSourceInstall(context.Background(), home, SourceInstallProduction, reservationRecoveryPath(t))
	if err != nil {
		t.Fatal(err)
	}
	root, err := sourceInstallRoot(home, SourceInstallProduction)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "unexpected"), []byte("retain\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseSourceInstall(
		context.Background(), home, SourceInstallProduction, reservation.Token, true,
	); !errors.Is(err, ErrReservationUnsafe) {
		t.Fatalf("release error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, sourceInstallReservationName)); err != nil {
		t.Fatalf("release removed marker before detecting nonempty root: %v", err)
	}
}

func reservationTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "Library", "Application Support"), 0o700); err != nil {
		t.Fatal(err)
	}
	return home
}

func reservationRecoveryPath(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(directory, "source-install-reservation.json")
}
