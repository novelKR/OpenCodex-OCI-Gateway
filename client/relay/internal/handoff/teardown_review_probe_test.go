package handoff

import (
	"context"
	"os"
	"testing"
)

// TestReviewedPackageClosureDigestExternalRoot is a test-only bridge used by
// the registry reconstruction tool. It prevents that independent Python
// verifier from approving a digest whose traversal order differs from the Go
// runtime that ultimately grants automatic-removal authority.
func TestReviewedPackageClosureDigestExternalRoot(t *testing.T) {
	root := os.Getenv("OPENCODEX_REVIEW_CLOSURE_ROOT")
	if root == "" {
		t.Skip("external review root is not configured")
	}
	digest, err := stableReviewedPackageClosureDigest(context.Background(), root)
	if err != nil {
		t.Fatalf("reviewed package closure: %v", err)
	}
	t.Logf("reviewed_closure_sha256=%s", digest)
}
