package handoff

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	maxExecutionTreeEntries = 50_000
	maxExecutionTreeBytes   = int64(512 << 20)
	maxExecutionFileBytes   = int64(128 << 20)
)

var errExecutionTreeExtendedACL = errors.New("execution tree has an extended ACL")

// stableExecutionTreeFingerprint binds the transitive package/module tree that
// Bun or npm will load. Two complete observations must agree so an update racing
// discovery cannot produce a mixed-generation proof.
func stableExecutionTreeFingerprint(root string) (string, error) {
	return stableExecutionTreeFingerprintContext(context.Background(), root)
}

func stableExecutionTreeFingerprintContext(ctx context.Context, root string) (string, error) {
	return stableExecutionTreeFingerprintPolicyContext(ctx, root, false)
}

func stableExecutionTreeFingerprintWithoutExtendedACLContext(ctx context.Context, root string) (string, error) {
	return stableExecutionTreeFingerprintPolicyContext(ctx, root, true)
}

func stableExecutionTreeFingerprintPolicyContext(ctx context.Context, root string, rejectExtendedACL bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	first, err := executionTreeFingerprintPolicyContext(ctx, root, rejectExtendedACL)
	if err != nil {
		return "", err
	}
	second, err := executionTreeFingerprintPolicyContext(ctx, root, rejectExtendedACL)
	if err != nil {
		return "", err
	}
	if subtle.ConstantTimeCompare([]byte(first), []byte(second)) != 1 {
		return "", ErrUnsafeNPMInstallation
	}
	return first, nil
}

func executionTreeFingerprintContext(ctx context.Context, root string) (string, error) {
	return executionTreeFingerprintPolicyContext(ctx, root, false)
}

func executionTreeFingerprintPolicyContext(ctx context.Context, root string, rejectExtendedACL bool) (string, error) {
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
	hash := sha256.New()
	entries := 0
	var totalBytes int64
	writeField := func(value string) {
		_, _ = hash.Write([]byte(value))
		_, _ = hash.Write([]byte{0})
	}
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
		if err != nil || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return ErrUnsafeNPMInstallation
		}
		relative = filepath.ToSlash(relative)
		info, err := os.Lstat(path)
		if err != nil {
			return ErrUnsafeNPMInstallation
		}
		if rejectExtendedACL {
			present, aclErr := hasExtendedACL(path)
			if aclErr != nil {
				return ErrUnsafeNPMInstallation
			}
			if present {
				return errExecutionTreeExtendedACL
			}
		}
		uid, ok := ownerUID(info)
		if !ok || (int(uid) != os.Geteuid() && uid != 0) {
			return ErrUnsafeNPMInstallation
		}
		writeField(relative)
		writeField(strconv.FormatUint(uint64(info.Mode()), 10))
		writeField(strconv.FormatUint(uint64(uid), 10))
		switch {
		case info.IsDir():
			if !safeExecutionDirectory(info) {
				return ErrUnsafeNPMInstallation
			}
			writeField("directory")
			return nil
		case info.Mode().IsRegular():
			if !safeExecutionRegular(info) || info.Size() < 0 || info.Size() > maxExecutionFileBytes || totalBytes > maxExecutionTreeBytes-info.Size() {
				return ErrUnsafeNPMInstallation
			}
			totalBytes += info.Size()
			writeField("file")
			writeField(strconv.FormatInt(info.Size(), 10))
			file, err := os.Open(path)
			if err != nil {
				return ErrUnsafeNPMInstallation
			}
			openedInfo, statErr := file.Stat()
			if statErr != nil || !safeExecutionRegular(openedInfo) || !os.SameFile(info, openedInfo) {
				_ = file.Close()
				return ErrUnsafeNPMInstallation
			}
			read, copyErr := copyWithContext(ctx, hash, file, maxExecutionFileBytes+1)
			closeErr := file.Close()
			if copyErr != nil || closeErr != nil || read != info.Size() {
				return ErrUnsafeNPMInstallation
			}
			return nil
		case info.Mode()&os.ModeSymlink != 0:
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil || !pathContainedBy(root, resolved) {
				return ErrUnsafeNPMInstallation
			}
			target, err := filepath.Rel(root, resolved)
			if err != nil || filepath.IsAbs(target) || target == ".." || strings.HasPrefix(target, ".."+string(filepath.Separator)) {
				return ErrUnsafeNPMInstallation
			}
			writeField("symlink")
			writeField(filepath.ToSlash(target))
			return nil
		default:
			return ErrUnsafeNPMInstallation
		}
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", err
		}
		if errors.Is(err, errExecutionTreeExtendedACL) {
			return "", errExecutionTreeExtendedACL
		}
		return "", ErrUnsafeNPMInstallation
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func copyWithContext(ctx context.Context, destination io.Writer, source io.Reader, maximum int64) (int64, error) {
	if maximum < 0 {
		return 0, ErrUnsafeNPMInstallation
	}
	buffer := make([]byte, 64<<10)
	var copied int64
	for copied < maximum {
		if err := ctx.Err(); err != nil {
			return copied, err
		}
		remaining := maximum - copied
		chunk := len(buffer)
		if remaining < int64(chunk) {
			chunk = int(remaining)
		}
		read, readErr := source.Read(buffer[:chunk])
		if read > 0 {
			if err := ctx.Err(); err != nil {
				return copied, err
			}
			written, writeErr := destination.Write(buffer[:read])
			copied += int64(written)
			if writeErr != nil {
				return copied, writeErr
			}
			if written != read {
				return copied, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return copied, nil
			}
			return copied, readErr
		}
		if read == 0 {
			return copied, io.ErrNoProgress
		}
	}
	return copied, nil
}

func safeExecutionDirectory(info os.FileInfo) bool {
	return info != nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 &&
		info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) == 0 && info.Mode().Perm()&0o022 == 0
}

func safeExecutionRegular(info os.FileInfo) bool {
	return info != nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 &&
		info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) == 0 && info.Mode().Perm()&0o022 == 0
}

func npmRootFromCLI(path string) (string, bool) {
	clean := filepath.Clean(path)
	if filepath.Base(clean) != "npm-cli.js" || filepath.Base(filepath.Dir(clean)) != "bin" {
		return "", false
	}
	root := filepath.Dir(filepath.Dir(clean))
	return root, filepath.IsAbs(root) && filepath.Base(root) == "npm"
}

func discoverBundledBun(packageRoot string) (string, string, bool) {
	return discoverBundledBunForDiscovery(packageRoot, false)
}

func discoverBundledBunForDiscovery(packageRoot string, allowRelaxed bool) (string, string, bool) {
	for _, name := range []string{"bun.exe", "bun"} {
		candidate := filepath.Join(packageRoot, "node_modules", "bun", "bin", name)
		resolved, fingerprint, relaxed, err := verifyDiscoveryExecutable(candidate)
		if err == nil && (allowRelaxed || !relaxed) && pathContainedBy(packageRoot, resolved) {
			return resolved, fingerprint, true
		}
	}
	return "", "", false
}
