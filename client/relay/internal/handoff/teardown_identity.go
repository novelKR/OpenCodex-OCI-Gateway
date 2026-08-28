package handoff

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// stableReviewedPackageClosureDigest binds publisher-reviewed package bytes
// while deliberately excluding installation-local UID, timestamps, and
// non-executable permission bits. Those facts remain enforced by the existing
// execution-tree policy and discovery-to-snapshot fingerprint.
func stableReviewedPackageClosureDigest(ctx context.Context, root string) (string, error) {
	first, err := reviewedPackageClosureDigest(ctx, root)
	if err != nil {
		return "", err
	}
	second, err := reviewedPackageClosureDigest(ctx, root)
	if err != nil {
		return "", err
	}
	if subtle.ConstantTimeCompare([]byte(first), []byte(second)) != 1 {
		return "", ErrUnsafeNPMInstallation
	}
	return first, nil
}

func reviewedPackageClosureDigest(ctx context.Context, root string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return "", ErrUnsafeNPMInstallation
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil || resolvedRoot != root {
		return "", ErrUnsafeNPMInstallation
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || !safeExecutionDirectory(rootInfo) {
		return "", ErrUnsafeNPMInstallation
	}

	digest := sha256.New()
	writeCanonicalIdentityField(digest, []byte("relay-reviewed-package-closure-v1"))
	entries := 0
	var totalBytes int64
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return ErrUnsafeNPMInstallation
		}
		entries++
		if entries > maxExecutionTreeEntries {
			return ErrUnsafeNPMInstallation
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || filepath.IsAbs(relative) || relative == ".." ||
			strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return ErrUnsafeNPMInstallation
		}
		relative = filepath.ToSlash(relative)
		info, err := os.Lstat(path)
		if err != nil {
			return ErrUnsafeNPMInstallation
		}
		uid, ok := ownerUID(info)
		if !ok || (int(uid) != os.Geteuid() && uid != 0) {
			return ErrUnsafeNPMInstallation
		}

		writeCanonicalIdentityField(digest, []byte(relative))
		if info.Mode().Perm()&0o111 != 0 {
			writeCanonicalIdentityField(digest, []byte{1})
		} else {
			writeCanonicalIdentityField(digest, []byte{0})
		}
		switch {
		case info.IsDir():
			if !safeExecutionDirectory(info) {
				return ErrUnsafeNPMInstallation
			}
			writeCanonicalIdentityField(digest, []byte("directory"))
			return nil
		case info.Mode().IsRegular():
			if !safeExecutionRegular(info) || info.Size() < 0 || info.Size() > maxExecutionFileBytes ||
				totalBytes > maxExecutionTreeBytes-info.Size() {
				return ErrUnsafeNPMInstallation
			}
			totalBytes += info.Size()
			writeCanonicalIdentityField(digest, []byte("file"))
			fileDigest, err := reviewedRegularFileDigest(ctx, path, info)
			if err != nil {
				return err
			}
			writeCanonicalIdentityField(digest, fileDigest)
			return nil
		case info.Mode()&os.ModeSymlink != 0:
			linkTarget, err := os.Readlink(path)
			if err != nil || linkTarget == "" || filepath.IsAbs(linkTarget) {
				return ErrUnsafeNPMInstallation
			}
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil || !pathContainedBy(root, resolved) {
				return ErrUnsafeNPMInstallation
			}
			writeCanonicalIdentityField(digest, []byte("symlink"))
			writeCanonicalIdentityField(digest, []byte(filepath.ToSlash(linkTarget)))
			return nil
		default:
			return ErrUnsafeNPMInstallation
		}
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", err
		}
		return "", ErrUnsafeNPMInstallation
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func reviewedRegularFileDigest(ctx context.Context, path string, expected os.FileInfo) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, ErrUnsafeNPMInstallation
	}
	openedInfo, statErr := file.Stat()
	uid, ownerOK := ownerUID(openedInfo)
	if statErr != nil || !safeExecutionRegular(openedInfo) || !ownerOK ||
		(int(uid) != os.Geteuid() && uid != 0) || !os.SameFile(expected, openedInfo) {
		_ = file.Close()
		return nil, ErrUnsafeNPMInstallation
	}
	hash := sha256.New()
	read, copyErr := copyWithContext(ctx, hash, file, maxExecutionFileBytes+1)
	closedErr := file.Close()
	if copyErr != nil || closedErr != nil || read != expected.Size() {
		return nil, ErrUnsafeNPMInstallation
	}
	return hash.Sum(nil), nil
}

func writeCanonicalIdentityField(destination interface{ Write([]byte) (int, error) }, value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = destination.Write(length[:])
	_, _ = destination.Write(value)
}
