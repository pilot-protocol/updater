// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"fmt"
	"strconv"
	"strings"
)

// Semver represents a parsed semantic version.
type Semver struct {
	Major int
	Minor int
	Patch int
	// Pre holds the prerelease identifiers that follow the first hyphen,
	// without the leading hyphen. It is empty for a plain release tag.
	// "v1.2.3-rc1" yields "rc1"; "v1.2.3" yields "".
	Pre string
}

// ParseSemver parses a version string like "v1.2.3" or "1.2.3" or "v1.2.3-rc1".
// It strips the "v" prefix and any "+build" metadata, keeps the numeric
// major.minor.patch triple, and retains the prerelease suffix in Pre.
func ParseSemver(s string) (Semver, error) {
	s = strings.TrimPrefix(s, "v")
	// Build metadata does not take part in version precedence.
	if idx := strings.IndexByte(s, '+'); idx >= 0 {
		s = s[:idx]
	}
	// Everything after the first hyphen is the prerelease suffix
	// (e.g. "-dirty", "-rc1", "-beta.2").
	var pre string
	if idx := strings.IndexByte(s, '-'); idx >= 0 {
		pre = s[idx+1:]
		s = s[:idx]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return Semver{}, fmt.Errorf("invalid semver: %q", s)
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return Semver{}, fmt.Errorf("invalid major: %w", err)
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return Semver{}, fmt.Errorf("invalid minor: %w", err)
	}
	patch, err := strconv.Atoi(parts[2])
	if err != nil {
		return Semver{}, fmt.Errorf("invalid patch: %w", err)
	}
	return Semver{Major: major, Minor: minor, Patch: patch, Pre: pre}, nil
}

// Compare returns -1, 0 or +1 as v sorts before, equal to, or after other.
// Ordering follows semantic-version precedence: the numeric triple is
// compared first, then the prerelease suffix. A version carrying a
// prerelease suffix sorts before the same version without one, so
// "1.2.3-rc1" < "1.2.3".
func (v Semver) Compare(other Semver) int {
	if c := compareInt(v.Major, other.Major); c != 0 {
		return c
	}
	if c := compareInt(v.Minor, other.Minor); c != 0 {
		return c
	}
	if c := compareInt(v.Patch, other.Patch); c != 0 {
		return c
	}
	return comparePrerelease(v.Pre, other.Pre)
}

// NewerThan returns true if v is strictly newer than other.
func (v Semver) NewerThan(other Semver) bool {
	return v.Compare(other) > 0
}

// String returns the version as "vMAJOR.MINOR.PATCH", with the prerelease
// suffix appended when present.
func (v Semver) String() string {
	if v.Pre != "" {
		return fmt.Sprintf("v%d.%d.%d-%s", v.Major, v.Minor, v.Patch, v.Pre)
	}
	return fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// comparePrerelease orders two prerelease suffixes. An empty suffix (a plain
// release) sorts after any non-empty one. Otherwise the suffixes are split on
// "." and compared identifier by identifier; when every shared identifier is
// equal, the suffix with more identifiers sorts later.
func comparePrerelease(a, b string) int {
	if a == b {
		return 0
	}
	if a == "" {
		return 1
	}
	if b == "" {
		return -1
	}
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		if c := comparePrereleaseIdent(as[i], bs[i]); c != 0 {
			return c
		}
	}
	return compareInt(len(as), len(bs))
}

// comparePrereleaseIdent orders two prerelease identifiers. All-digit
// identifiers are compared numerically and sort before alphanumeric ones;
// any other pair is compared bytewise.
func comparePrereleaseIdent(a, b string) int {
	an, aNumeric := numericIdent(a)
	bn, bNumeric := numericIdent(b)
	switch {
	case aNumeric && bNumeric:
		return compareInt(an, bn)
	case aNumeric:
		return -1
	case bNumeric:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

// numericIdent reports whether s consists solely of digits, returning its
// numeric value when it does.
func numericIdent(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

func compareInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
