package refdiff

import (
	"cmp"
	"strings"
)

// VersionLess orders versions as semantic versioning ranks them, with a full tie-break: its
// release numbers, a prerelease before its release, then the build metadata, then the strings
// themselves, so no two different versions are ever equal and every sort of them is the same.
func VersionLess(a, b string) bool { return VersionCompare(a, b) < 0 }

// VersionCompare is -1, 0 or +1 as a ranks before, as or after b. Precedence is semantic
// versioning's: release numbers compared numerically, a missing one 0 (1.2 ranks as 1.2.0); a
// prerelease (after the first "-") before its release, its dot-separated identifiers compared in
// turn (numeric ones numerically and before alphanumeric ones, those in ASCII order), and of two
// whose identifiers agree as far as both go, the one with fewer first (alpha before alpha.1).
// Build metadata (after the first "+") takes no part in precedence; it, compared as a
// prerelease's identifiers are, then the strings themselves (1.2 before 1.2.0) break a tie.
func VersionCompare(a, b string) int {
	amain, abuild, _ := strings.Cut(a, "+")
	bmain, bbuild, _ := strings.Cut(b, "+")
	arel, apre, apreOK := strings.Cut(amain, "-")
	brel, bpre, bpreOK := strings.Cut(bmain, "-")
	if c := compareRelease(arel, brel); c != 0 {
		return c
	}
	switch {
	case apreOK && !bpreOK:
		return -1
	case !apreOK && bpreOK:
		return 1
	}
	if c := compareIdentifiers(apre, bpre); c != 0 {
		return c
	}
	if c := compareIdentifiers(abuild, bbuild); c != 0 {
		return c
	}
	return cmp.Compare(a, b)
}

// compareRelease compares dotted release numbers, a missing one 0; a part that is no number
// ranks after every number, its kind among themselves in ASCII order.
func compareRelease(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := range max(len(as), len(bs)) {
		x, y := "0", "0"
		if i < len(as) {
			x = as[i]
		}
		if i < len(bs) {
			y = bs[i]
		}
		if c := compareIdentifier(x, y); c != 0 {
			return c
		}
	}
	return 0
}

// compareIdentifiers compares two dot-separated lists of identifiers in turn; of two that agree
// as far as both go, the shorter ranks first. Empty is no identifiers.
func compareIdentifiers(a, b string) int {
	var as, bs []string
	if a != "" {
		as = strings.Split(a, ".")
	}
	if b != "" {
		bs = strings.Split(b, ".")
	}
	for i := range min(len(as), len(bs)) {
		if c := compareIdentifier(as[i], bs[i]); c != 0 {
			return c
		}
	}
	return cmp.Compare(len(as), len(bs))
}

// compareIdentifier compares two identifiers: numbers numerically, before anything else, the
// rest in ASCII order.
func compareIdentifier(a, b string) int {
	an, bn := numeric(a), numeric(b)
	switch {
	case an && bn:
		// As numbers, however long: leading zeros aside, the longer is the larger.
		a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
		if c := cmp.Compare(len(a), len(b)); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	case an:
		return -1
	case bn:
		return 1
	}
	return cmp.Compare(a, b)
}

// numeric reports an identifier of digits alone.
func numeric(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}
