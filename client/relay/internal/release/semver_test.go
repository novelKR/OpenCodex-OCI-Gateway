package release

import "testing"

func TestSemanticVersionOrdering(t *testing.T) {
	ordered := []string{
		"0.3.7",
		"0.3.8-rc.5",
		"0.3.8-rc.6",
		"0.3.8",
		"0.4.0-alpha",
		"0.4.0-alpha.1",
		"0.4.0-beta",
	}
	for index := 1; index < len(ordered); index++ {
		left, err := ParseSemanticVersion(ordered[index-1])
		if err != nil {
			t.Fatal(err)
		}
		right, err := ParseSemanticVersion(ordered[index])
		if err != nil {
			t.Fatal(err)
		}
		if left.Compare(right) >= 0 || right.Compare(left) <= 0 {
			t.Fatalf("expected %s < %s", left, right)
		}
	}
}

func TestSemanticVersionRejectsNonCanonicalForms(t *testing.T) {
	for _, value := range []string{
		"", "v0.3.8", "0.3", "0.3.8+build.1", "0.03.8", "0.3.8-", "0.3.8-rc.01",
		"0.3.8-rc_1", " 0.3.8", "0.3.8 ",
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := ParseSemanticVersion(value); err == nil {
				t.Fatalf("accepted non-canonical version %q", value)
			}
		})
	}
}

func TestSelectCandidateUsesSemVerAndChannel(t *testing.T) {
	no := false
	releases := []githubRelease{
		{ID: 10, TagName: "0.3.8-rc.6", Draft: &no},
		{ID: 11, TagName: "0.3.7", Draft: &no},
		{ID: 12, TagName: "0.3.8", Draft: &no},
		{ID: 13, TagName: "garbage", Draft: &no},
	}
	stable, version, found, valid := selectCandidate(releases, UpdateChannelStable)
	if !valid || !found || stable.ID != 12 || version.String() != "0.3.8" {
		t.Fatalf("stable = %#v, %q, found=%v valid=%v", stable, version, found, valid)
	}
	preview, version, found, valid := selectCandidate(releases[:2], UpdateChannelPreview)
	if !valid || !found || preview.ID != 10 || version.String() != "0.3.8-rc.6" {
		t.Fatalf("preview = %#v, %q, found=%v valid=%v", preview, version, found, valid)
	}
}

func TestSelectCandidateRejectsDuplicateCanonicalVersion(t *testing.T) {
	no := false
	_, _, found, valid := selectCandidate([]githubRelease{
		{ID: 1, TagName: "0.3.8", Draft: &no},
		{ID: 2, TagName: "0.3.8", Draft: &no},
	}, UpdateChannelPreview)
	if valid || found {
		t.Fatalf("duplicate version accepted: found=%v valid=%v", found, valid)
	}
}
