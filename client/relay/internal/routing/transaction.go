package routing

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// TransactionPath is reserved for the non-secret controller journal that
// records an in-progress multi-file routing change. The resident relay does
// not derive routing decisions from its contents, but validates its bounded
// shape before trusting an applying state so corruption cannot masquerade as
// an active controller transaction.
func TransactionPath(configPath string) string {
	return filepath.Clean(configPath) + ".routing-transaction.json"
}

// TransactionPath returns the path paired with this Store's config binding.
func (s *Store) TransactionPath() string {
	if s == nil {
		return ""
	}
	return TransactionPath(s.configPath)
}

// HasPendingTransaction reports whether a controller transaction journal is
// present. It must be an owner-only regular file and a valid non-secret
// journal, so a foreign user or malformed file cannot manipulate admission by
// creating a path next to relay.json.
func (s *Store) HasPendingTransaction() (bool, error) {
	if s == nil {
		return false, errors.New("routing store is nil")
	}
	path := s.TransactionPath()
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect routing transaction: %w", err)
	}
	if err := validateOwnedRegular(info, path); err != nil {
		return false, fmt.Errorf("inspect routing transaction: %w", err)
	}
	if err := ValidateTransaction(s.configPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// The controller may have atomically removed its journal between
			// Lstat and validation immediately before saving a final state.
			return false, nil
		}
		return false, fmt.Errorf("validate routing transaction: %w", err)
	}
	return true, nil
}
