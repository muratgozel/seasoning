package selfupdate

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// DevVersion is the version string of local builds (`make install`).
const DevVersion = "dev"

// maxVersionLen bounds tags taken from the network or the command line.
const maxVersionLen = 64

type semver struct {
	major, minor, patch uint64
	pre                 []string // prerelease identifiers; empty for a release
}

var identRe = regexp.MustCompile(`^[0-9A-Za-z-]+$`)

// parseVersion accepts MAJOR.MINOR.PATCH with an optional leading "v",
// optional -prerelease and optional +build metadata (semver 2.0.0).
func parseVersion(s string) (semver, error) {
	bad := func() (semver, error) {
		return semver{}, fmt.Errorf("%q is not a semantic version (expected vX.Y.Z)", s)
	}
	if s == "" || len(s) > maxVersionLen {
		return bad()
	}
	rest := strings.TrimPrefix(s, "v")
	if i := strings.IndexByte(rest, '+'); i >= 0 {
		if !validIdents(rest[i+1:], false) {
			return bad()
		}
		rest = rest[:i]
	}
	var v semver
	if i := strings.IndexByte(rest, '-'); i >= 0 {
		if !validIdents(rest[i+1:], true) {
			return bad()
		}
		v.pre = strings.Split(rest[i+1:], ".")
		rest = rest[:i]
	}
	parts := strings.Split(rest, ".")
	if len(parts) != 3 {
		return bad()
	}
	nums := make([]uint64, 3)
	for i, p := range parts {
		n, ok := numericIdent(p)
		if !ok {
			return bad()
		}
		nums[i] = n
	}
	v.major, v.minor, v.patch = nums[0], nums[1], nums[2]
	return v, nil
}

// numericIdent parses a non-negative integer without leading zeros.
func numericIdent(s string) (uint64, bool) {
	if s == "" || (len(s) > 1 && s[0] == '0') {
		return 0, false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(s, 10, 64)
	return n, err == nil
}

func validIdents(s string, pre bool) bool {
	if s == "" {
		return false
	}
	for _, id := range strings.Split(s, ".") {
		if !identRe.MatchString(id) {
			return false
		}
		if pre && isDigits(id) && len(id) > 1 && id[0] == '0' {
			return false
		}
	}
	return true
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

func (a semver) compare(b semver) int {
	for _, d := range [][2]uint64{{a.major, b.major}, {a.minor, b.minor}, {a.patch, b.patch}} {
		if d[0] != d[1] {
			if d[0] < d[1] {
				return -1
			}
			return 1
		}
	}
	// A release sorts after any of its prereleases.
	switch {
	case len(a.pre) == 0 && len(b.pre) == 0:
		return 0
	case len(a.pre) == 0:
		return 1
	case len(b.pre) == 0:
		return -1
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		if c := comparePreIdent(a.pre[i], b.pre[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(a.pre) < len(b.pre):
		return -1
	case len(a.pre) > len(b.pre):
		return 1
	}
	return 0
}

func comparePreIdent(a, b string) int {
	an, aNum := numericIdent(a)
	bn, bNum := numericIdent(b)
	switch {
	case aNum && bNum:
		switch {
		case an < bn:
			return -1
		case an > bn:
			return 1
		}
		return 0
	case aNum:
		return -1 // numeric identifiers sort before alphanumeric ones
	case bNum:
		return 1
	}
	return strings.Compare(a, b)
}

// CompareVersions orders two versions (tags with or without a leading "v").
// "dev" is older than any release. Malformed versions are an error.
func CompareVersions(a, b string) (int, error) {
	if a == DevVersion || b == DevVersion {
		other := b
		if a != DevVersion {
			other = a
		}
		if other != DevVersion {
			if _, err := parseVersion(other); err != nil {
				return 0, err
			}
		}
		switch {
		case a == b:
			return 0, nil
		case a == DevVersion:
			return -1, nil
		}
		return 1, nil
	}
	va, err := parseVersion(a)
	if err != nil {
		return 0, err
	}
	vb, err := parseVersion(b)
	if err != nil {
		return 0, err
	}
	return va.compare(vb), nil
}

// gitDescribeRe matches the suffix `git describe --tags --dirty` appends to
// builds past a tag (`v0.3.0-4-gabc1234`, `v0.3.0-dirty`).
var gitDescribeRe = regexp.MustCompile(`(-[0-9]+-g[0-9a-f]{4,}(-dirty)?|-dirty)$`)

// IsDevBuild reports whether v is a local build rather than a published
// release: "dev", anything that is not semver (a bare commit hash from
// `git describe --always`), or a git-describe version past a tag. Such
// builds never get update hints and `update` always moves them to a release.
func IsDevBuild(v string) bool {
	if v == "" || v == DevVersion {
		return true
	}
	if _, err := parseVersion(v); err != nil {
		return true
	}
	return gitDescribeRe.MatchString(v)
}

// DisplayVersion strips the leading "v" of a tag: v0.3.0 → 0.3.0. This is
// also the file name used in the versions dir.
func DisplayVersion(v string) string { return strings.TrimPrefix(v, "v") }

// NormalizeTag validates a user-supplied version (0.3.0 or v0.3.0) and returns
// the release tag (v0.3.0).
func NormalizeTag(v string) (string, error) {
	v = strings.TrimSpace(v)
	if _, err := parseVersion(v); err != nil {
		return "", err
	}
	return "v" + DisplayVersion(v), nil
}

// validVersionName reports whether name may be a file in the versions dir.
// Prune deletes only such files, so a misconfigured SEASONING_VERSIONS_DIR
// never loses unrelated files.
func validVersionName(name string) bool {
	if name == DevVersion {
		return true
	}
	if strings.HasPrefix(name, "v") {
		return false
	}
	_, err := parseVersion(name)
	return err == nil
}
