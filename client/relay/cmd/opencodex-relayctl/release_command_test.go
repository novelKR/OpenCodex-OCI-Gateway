package main

import (
	"strings"
	"testing"
)

func TestRelayctlUsageDocumentsFixedReleaseCheckContract(t *testing.T) {
	var output strings.Builder
	writeUsage(&output)
	usage := output.String()
	for _, required := range []string{
		"release check", "--channel stable|preview", "--current-version VERSION",
		"--public-key ABSOLUTE_PATH", "--json",
	} {
		if !strings.Contains(usage, required) {
			t.Fatalf("usage omits %q: %q", required, usage)
		}
	}
	for _, forbidden := range []string{"--repository", "--api-url", "--api-base-url"} {
		if strings.Contains(usage, forbidden) {
			t.Fatalf("usage exposes production trust boundary %q: %q", forbidden, usage)
		}
	}
}
