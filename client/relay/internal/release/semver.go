package release

import (
	"fmt"
	"strings"
)

// SemanticVersion is the release contract's strict SemVer subset. Build
// metadata and a leading "v" are deliberately excluded so one published tag
// has exactly one ordering and one canonical spelling.
type SemanticVersion struct {
	raw        string
	core       [3]string
	prerelease []string
}

func ParseSemanticVersion(value string) (SemanticVersion, error) {
	if value == "" || strings.Contains(value, "+") {
		return SemanticVersion{}, fmt.Errorf("version is not strict SemVer: %q", value)
	}
	core, prerelease, hasPrerelease := strings.Cut(value, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return SemanticVersion{}, fmt.Errorf("version is not strict SemVer: %q", value)
	}
	var parsed SemanticVersion
	parsed.raw = value
	for index, part := range parts {
		if !validNumericIdentifier(part) {
			return SemanticVersion{}, fmt.Errorf("version is not strict SemVer: %q", value)
		}
		parsed.core[index] = part
	}
	if !hasPrerelease {
		return parsed, nil
	}
	if prerelease == "" {
		return SemanticVersion{}, fmt.Errorf("version is not strict SemVer: %q", value)
	}
	for _, identifier := range strings.Split(prerelease, ".") {
		if !validPrereleaseIdentifier(identifier) {
			return SemanticVersion{}, fmt.Errorf("version is not strict SemVer: %q", value)
		}
		parsed.prerelease = append(parsed.prerelease, identifier)
	}
	return parsed, nil
}

func (v SemanticVersion) String() string { return v.raw }

func (v SemanticVersion) IsPrerelease() bool { return len(v.prerelease) != 0 }

// Compare returns -1, 0, or 1 using SemVer precedence rules.
func (v SemanticVersion) Compare(other SemanticVersion) int {
	for index := range v.core {
		if comparison := compareNumericIdentifier(v.core[index], other.core[index]); comparison != 0 {
			return comparison
		}
	}
	if len(v.prerelease) == 0 && len(other.prerelease) == 0 {
		return 0
	}
	if len(v.prerelease) == 0 {
		return 1
	}
	if len(other.prerelease) == 0 {
		return -1
	}
	limit := min(len(v.prerelease), len(other.prerelease))
	for index := 0; index < limit; index++ {
		left := v.prerelease[index]
		right := other.prerelease[index]
		leftNumeric := isNumericIdentifier(left)
		rightNumeric := isNumericIdentifier(right)
		switch {
		case leftNumeric && rightNumeric:
			if comparison := compareNumericIdentifier(left, right); comparison != 0 {
				return comparison
			}
		case leftNumeric:
			return -1
		case rightNumeric:
			return 1
		case left < right:
			return -1
		case left > right:
			return 1
		}
	}
	switch {
	case len(v.prerelease) < len(other.prerelease):
		return -1
	case len(v.prerelease) > len(other.prerelease):
		return 1
	default:
		return 0
	}
}

func validPrereleaseIdentifier(value string) bool {
	if value == "" {
		return false
	}
	numeric := true
	for _, character := range value {
		if (character < '0' || character > '9') &&
			(character < 'A' || character > 'Z') &&
			(character < 'a' || character > 'z') && character != '-' {
			return false
		}
		if character < '0' || character > '9' {
			numeric = false
		}
	}
	return !numeric || len(value) == 1 || value[0] != '0'
}

func isNumericIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func compareNumericIdentifier(left, right string) int {
	switch {
	case len(left) < len(right):
		return -1
	case len(left) > len(right):
		return 1
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}
