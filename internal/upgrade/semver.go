package upgrade

import "strings"

// version is a semantic version: major.minor.patch with an optional
// dot-separated prerelease. Build metadata is ignored when comparing.
type version struct {
	major, minor, patch int
	pre                 string
}

// parseVersion parses "1.2.3", "v1.2.3" and "1.2.3-rc.1". The second result is
// false for anything else, which is what keeps an unorderable version from
// being compared with a release.
func parseVersion(s string) (version, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return version{}, false
	}
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	pre := ""
	if i := strings.IndexByte(s, '-'); i >= 0 {
		pre = s[i+1:]
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return version{}, false
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, ok := parseNumber(p)
		if !ok {
			return version{}, false
		}
		nums[i] = n
	}
	return version{major: nums[0], minor: nums[1], patch: nums[2], pre: pre}, true
}

func parseNumber(s string) (int, bool) {
	if s == "" || len(s) > 9 {
		return 0, false
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	return n, true
}

// compare orders two parsed versions: negative when v is older, positive when
// it is newer, zero when they are equal. A prerelease sorts before the release
// with the same numbers, and identifiers are compared numerically when both
// sides are numeric and lexically otherwise.
func (v version) compare(o version) int {
	if c := compareInt(v.major, o.major); c != 0 {
		return c
	}
	if c := compareInt(v.minor, o.minor); c != 0 {
		return c
	}
	if c := compareInt(v.patch, o.patch); c != 0 {
		return c
	}
	return comparePrerelease(v.pre, o.pre)
}

func compareInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func comparePrerelease(a, b string) int {
	if a == b {
		return 0
	}
	// A release without a prerelease outranks any prerelease of the same
	// numbers: 1.0.0 > 1.0.0-rc.1.
	if a == "" {
		return 1
	}
	if b == "" {
		return -1
	}
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		an, aok := parseNumber(as[i])
		bn, bok := parseNumber(bs[i])
		switch {
		case aok && bok:
			if c := compareInt(an, bn); c != 0 {
				return c
			}
		case aok != bok:
			// Numeric identifiers rank below alphanumeric ones.
			if aok {
				return -1
			}
			return 1
		default:
			if c := strings.Compare(as[i], bs[i]); c != 0 {
				return c
			}
		}
	}
	return compareInt(len(as), len(bs))
}
