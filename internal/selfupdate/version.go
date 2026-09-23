package selfupdate

import (
	"regexp"
	"strings"
)

var releaseVersion = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)
var describeVersion = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+.*-[0-9]+-g[0-9a-f]{7,40}(?:-dirty)?$`)

// IsRelease reports whether version belongs to the numbered release sequence.
// Unversioned, dirty, and snapshot builds are never replaced automatically.
func IsRelease(version string) bool {
	parts := releaseVersion.FindStringSubmatch(version)
	if parts == nil || describeVersion.MatchString(version) || strings.Contains(version, "-next") || strings.HasSuffix(version, "-dirty") {
		return false
	}
	for _, part := range strings.Split(parts[4], ".") {
		if numericIdentifier(part) && len(part) > 1 && part[0] == '0' {
			return false
		}
	}
	return true
}

// Newer compares numbered versions, including semantic prerelease ordering.
// Unknown build identifiers are not guessed to be version zero.
func Newer(current, candidate string) bool {
	if !IsRelease(current) || !IsRelease(candidate) {
		return false
	}
	a, b := releaseVersion.FindStringSubmatch(candidate), releaseVersion.FindStringSubmatch(current)
	for i := 1; i <= 3; i++ {
		if d := compareNumber(a[i], b[i]); d != 0 {
			return d > 0
		}
	}
	if a[4] == b[4] {
		return false
	}
	if a[4] == "" {
		return true
	}
	if b[4] == "" {
		return false
	}
	aa, bb := strings.Split(a[4], "."), strings.Split(b[4], ".")
	for i := 0; i < len(aa) && i < len(bb); i++ {
		if aa[i] == bb[i] {
			continue
		}
		an, bn := numericIdentifier(aa[i]), numericIdentifier(bb[i])
		if an && bn {
			return compareNumber(aa[i], bb[i]) > 0
		}
		if an {
			return false
		}
		if bn {
			return true
		}
		return aa[i] > bb[i]
	}
	return len(aa) > len(bb)
}

func compareNumber(a, b string) int {
	if len(a) != len(b) {
		if len(a) > len(b) {
			return 1
		}
		return -1
	}
	return strings.Compare(a, b)
}

func numericIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
