//go:build darwin

package lifecyclelock

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

const maximumReservationBytes = 4 << 10

type sourceInstallReservationRecord struct {
	SchemaVersion int                `json:"schema_version"`
	Scope         SourceInstallScope `json:"scope"`
	Token         string             `json:"token"`
}

func reserveSourceInstall(ctx context.Context, home string, scope SourceInstallScope, recoveryPath string) (SourceInstallReservation, error) {
	root, err := sourceInstallRoot(home, scope)
	if err != nil {
		return SourceInstallReservation{}, err
	}
	lock, err := Acquire(ctx, home)
	if err != nil {
		return SourceInstallReservation{}, err
	}
	defer lock.Close()
	if err := requireStandaloneAnchorAbsent(home); err != nil {
		return SourceInstallReservation{}, err
	}
	if err := validateSourceInstallReservation(home, ""); !errors.Is(err, ErrReservationBusy) && err != nil {
		return SourceInstallReservation{}, err
	} else if errors.Is(err, ErrReservationBusy) {
		return SourceInstallReservation{}, err
	}
	rootExists, rootCreated, err := inspectReservationRoot(home, scope, true)
	if err != nil || !rootExists {
		return SourceInstallReservation{}, ErrReservationUnsafe
	}
	cleanupRoot := func() {
		if rootCreated {
			_ = os.Remove(root)
		}
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		cleanupRoot()
		return SourceInstallReservation{}, ErrReservationUnsafe
	}
	record := sourceInstallReservationRecord{
		SchemaVersion: 1,
		Scope:         scope,
		Token:         hex.EncodeToString(tokenBytes),
	}
	reservation := SourceInstallReservation{
		SchemaVersion: 1,
		Scope:         scope,
		Token:         record.Token,
		RootCreated:   rootCreated,
	}
	payload, err := json.Marshal(record)
	if err != nil {
		cleanupRoot()
		return SourceInstallReservation{}, ErrReservationUnsafe
	}
	payload = append(payload, '\n')
	if err := writeSourceInstallRecovery(recoveryPath, reservation); err != nil {
		cleanupRoot()
		return SourceInstallReservation{}, ErrReservationUnsafe
	}
	recoveryWritten := true
	cleanupRecovery := func() {
		if recoveryWritten {
			_ = os.Remove(recoveryPath)
			_ = syncDirectory(filepath.Dir(recoveryPath))
			recoveryWritten = false
		}
	}
	marker := filepath.Join(root, sourceInstallReservationName)
	fd, err := syscall.Open(marker, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		cleanupRecovery()
		cleanupRoot()
		if errors.Is(err, syscall.EEXIST) {
			return SourceInstallReservation{}, ErrReservationBusy
		}
		return SourceInstallReservation{}, ErrReservationUnsafe
	}
	file := os.NewFile(uintptr(fd), marker)
	written := false
	defer func() {
		_ = file.Close()
		if !written {
			_ = os.Remove(marker)
			cleanupRecovery()
			cleanupRoot()
		}
	}()
	if _, err := file.Write(payload); err != nil || file.Sync() != nil || file.Close() != nil || syncDirectory(root) != nil {
		return SourceInstallReservation{}, ErrReservationUnsafe
	}
	written = true
	return reservation, nil
}

func writeSourceInstallRecovery(path string, reservation SourceInstallReservation) error {
	clean := filepath.Clean(path)
	if path == "" || clean != path || !filepath.IsAbs(clean) || filepath.Base(clean) == "." {
		return ErrReservationUnsafe
	}
	parent := filepath.Dir(clean)
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return ErrReservationUnsafe
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return ErrReservationUnsafe
	}
	payload, err := json.Marshal(reservation)
	if err != nil || len(payload) > maximumReservationBytes-1 {
		return ErrReservationUnsafe
	}
	payload = append(payload, '\n')
	fd, err := syscall.Open(clean, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return ErrReservationUnsafe
	}
	file := os.NewFile(uintptr(fd), clean)
	written := false
	defer func() {
		_ = file.Close()
		if !written {
			_ = os.Remove(clean)
		}
	}()
	if _, err := file.Write(payload); err != nil || file.Sync() != nil || file.Close() != nil || syncDirectory(parent) != nil {
		return ErrReservationUnsafe
	}
	written = true
	return nil
}

func releaseSourceInstall(ctx context.Context, home string, scope SourceInstallScope, token string, removeCreatedRoot bool) error {
	root, err := sourceInstallRoot(home, scope)
	if err != nil || !validReservationToken(token) {
		return ErrReservationUnsafe
	}
	lock, err := Acquire(ctx, home)
	if err != nil {
		return err
	}
	defer lock.Close()
	rootExists, _, err := inspectReservationRoot(home, scope, false)
	if err != nil || !rootExists {
		return ErrReservationUnsafe
	}
	marker := filepath.Join(root, sourceInstallReservationName)
	record, err := readSourceInstallReservation(marker)
	if err != nil || record.Scope != scope || subtle.ConstantTimeCompare([]byte(record.Token), []byte(token)) != 1 {
		return ErrReservationUnsafe
	}
	if removeCreatedRoot {
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 1 || entries[0].Name() != sourceInstallReservationName {
			// Keep the marker authoritative if the root is no longer empty. A
			// caller must never discover that only after deleting its durable
			// admission evidence.
			return ErrReservationUnsafe
		}
	}
	if err := os.Remove(marker); err != nil {
		return ErrReservationUnsafe
	}
	if err := syncDirectory(root); err != nil {
		return ErrReservationUnsafe
	}
	if removeCreatedRoot {
		if err := os.Remove(root); err != nil && !errors.Is(err, os.ErrNotExist) {
			// A non-empty root remains a recognized Relay asset and therefore
			// keeps standalone removal fail-closed after an incomplete rollback.
			return ErrReservationUnsafe
		}
		if err := syncDirectory(filepath.Dir(root)); err != nil {
			return ErrReservationUnsafe
		}
	}
	return nil
}

func validateSourceInstallReservation(home, token string) error {
	reservations := 0
	for _, scope := range []SourceInstallScope{SourceInstallProduction, SourceInstallLocalDevelopment} {
		root, err := sourceInstallRoot(home, scope)
		if err != nil {
			return err
		}
		rootExists, _, err := inspectReservationRoot(home, scope, false)
		if err != nil {
			return ErrReservationUnsafe
		}
		if !rootExists {
			continue
		}
		marker := filepath.Join(root, sourceInstallReservationName)
		record, err := readSourceInstallReservation(marker)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return ErrReservationUnsafe
		}
		if record.Scope != scope {
			return ErrReservationUnsafe
		}
		reservations++
		if !validReservationToken(token) || subtle.ConstantTimeCompare([]byte(record.Token), []byte(token)) != 1 {
			return ErrReservationBusy
		}
	}
	if reservations > 1 {
		return ErrReservationUnsafe
	}
	return nil
}

func sourceInstallRoot(home string, scope SourceInstallScope) (string, error) {
	clean := filepath.Clean(home)
	if !filepath.IsAbs(clean) {
		return "", ErrReservationUnsafe
	}
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil || !filepath.IsAbs(resolved) {
		return "", ErrReservationUnsafe
	}
	suffix := "relay"
	if scope == SourceInstallLocalDevelopment {
		suffix = "relay-dev"
	} else if scope != SourceInstallProduction {
		return "", ErrReservationUnsafe
	}
	return filepath.Join(filepath.Clean(resolved), ".local", "lib", "opencodex-relay", suffix), nil
}

func inspectReservationRoot(home string, scope SourceInstallScope, create bool) (bool, bool, error) {
	root, err := sourceInstallRoot(home, scope)
	if err != nil {
		return false, false, ErrReservationUnsafe
	}
	resolvedHome, err := filepath.EvalSymlinks(filepath.Clean(home))
	if err != nil || !filepath.IsAbs(resolvedHome) {
		return false, false, ErrReservationUnsafe
	}
	relative, err := filepath.Rel(resolvedHome, root)
	if err != nil || relative == "." || relative == ".." || filepath.IsAbs(relative) ||
		len(relative) >= 3 && relative[:3] == ".."+string(filepath.Separator) {
		return false, false, ErrReservationUnsafe
	}
	current := filepath.Clean(resolvedHome)
	rootCreated := false
	components := splitPath(relative)
	for index, component := range components {
		current = filepath.Join(current, component)
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			if !create {
				return false, false, nil
			}
			if err := os.Mkdir(current, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return false, rootCreated, ErrReservationUnsafe
			}
			if err := syncDirectory(filepath.Dir(current)); err != nil {
				return false, rootCreated, ErrReservationUnsafe
			}
			info, statErr = os.Lstat(current)
			if index == len(components)-1 {
				rootCreated = true
			}
		}
		if statErr != nil || info == nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return false, rootCreated, ErrReservationUnsafe
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0o022 != 0 {
			return false, rootCreated, ErrReservationUnsafe
		}
		if index == len(components)-1 && info.Mode().Perm() != 0o700 {
			return false, rootCreated, ErrReservationUnsafe
		}
	}
	return true, rootCreated, nil
}

func splitPath(path string) []string {
	var components []string
	for path != "." && path != "" {
		directory, leaf := filepath.Split(path)
		if leaf != "" {
			components = append([]string{leaf}, components...)
		}
		path = filepath.Clean(directory)
		if path == string(filepath.Separator) {
			break
		}
	}
	return components
}

func syncDirectory(path string) error {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	return file.Sync()
}

func readSourceInstallReservation(path string) (sourceInstallReservationRecord, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return sourceInstallReservationRecord{}, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() <= 0 || info.Size() > maximumReservationBytes {
		return sourceInstallReservationRecord{}, ErrReservationUnsafe
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return sourceInstallReservationRecord{}, ErrReservationUnsafe
	}
	decoder := json.NewDecoder(io.LimitReader(file, maximumReservationBytes))
	decoder.DisallowUnknownFields()
	var record sourceInstallReservationRecord
	if decoder.Decode(&record) != nil || record.SchemaVersion != 1 || !validReservationToken(record.Token) ||
		(record.Scope != SourceInstallProduction && record.Scope != SourceInstallLocalDevelopment) {
		return sourceInstallReservationRecord{}, ErrReservationUnsafe
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return sourceInstallReservationRecord{}, ErrReservationUnsafe
	}
	return record, nil
}

func requireStandaloneAnchorAbsent(home string) error {
	lockPath, err := Path(home)
	if err != nil {
		return ErrReservationUnsafe
	}
	anchor := filepath.Join(filepath.Dir(lockPath), "standalone-native")
	for _, path := range []string{anchor, anchor + ".open-codex-removal.json"} {
		if _, err := os.Lstat(path); err == nil || !errors.Is(err, os.ErrNotExist) {
			return ErrReservationBusy
		}
	}
	return nil
}

func validReservationToken(token string) bool {
	if len(token) != 64 {
		return false
	}
	_, err := hex.DecodeString(token)
	return err == nil
}
