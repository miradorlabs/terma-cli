package refdiff

import (
	"strconv"
	"strings"
)

// VersionLess orders versions by their numbers, a prerelease before its release.
func VersionLess(a, b string) bool {
	ar, apre, _ := strings.Cut(a, "-")
	br, bpre, _ := strings.Cut(b, "-")
	as, bs := strings.Split(ar, "."), strings.Split(br, ".")
	for i := range max(len(as), len(bs)) {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			return x < y
		}
	}
	switch {
	case apre == bpre:
		return false
	case apre == "":
		return false
	case bpre == "":
		return true
	}
	return apre < bpre
}
