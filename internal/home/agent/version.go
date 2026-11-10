package agent

import (
	"fmt"
	"regexp"
	"strconv"
)

var versionRE = regexp.MustCompile(`version (\d+)\.(\d+)\.(\d+)`)

type appleVersion [3]int

func parseVersion(out []byte) (appleVersion, bool) {
	m := versionRE.FindSubmatch(out)
	if m == nil {
		return appleVersion{}, false
	}
	var v appleVersion
	for i := range v {
		n, err := strconv.Atoi(string(m[i+1]))
		if err != nil {
			return appleVersion{}, false
		}
		v[i] = n
	}
	return v, true
}

func (v appleVersion) String() string { return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]) }

// cmp is -1, 0 or 1.
func (v appleVersion) cmp(o appleVersion) int {
	for i := range v {
		switch {
		case v[i] < o[i]:
			return -1
		case v[i] > o[i]:
			return 1
		}
	}
	return 0
}

// CompareVersions is -1, 0 or 1 for versions in the "1.6.1" form; ok is false when either is not one.
func CompareVersions(a, b string) (cmp int, ok bool) {
	va, oka := parseVersion([]byte("version " + a))
	vb, okb := parseVersion([]byte("version " + b))
	return va.cmp(vb), oka && okb
}

// NormalizeVersion is a version in the "1.6.1" form as three numbers ("0.0.0" when it is none).
func NormalizeVersion(v string) string {
	p, _ := parseVersion([]byte("version " + v))
	return p.String()
}
