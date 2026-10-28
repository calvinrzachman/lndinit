package main

import (
	"fmt"
	"regexp"
	"strconv"
)

var (
	// lndTagRe matches an lnd release tag, either a final release such as
	// v0.20.5-beta or a release candidate such as v0.20.5-beta.rc1.
	//
	// Only canonical numbers are accepted: no leading zeros, at most four
	// digits, and release candidates start at rc1. Otherwise one release
	// could be spelled several ways (v0.20.5 and v0.20.05) and be planned
	// once per spelling.
	lndTagRe = regexp.MustCompile(
		`^v(0|[1-9]\d{0,3})\.(0|[1-9]\d{0,3})\.(0|[1-9]\d{0,3})` +
			`-beta(?:\.rc([1-9]\d{0,3}))?$`,
	)

	// lineRe extracts the major line from any lnd module version,
	// including pseudo-versions such as
	// v0.21.0-beta.rc2.0.20260717013942-f02cf4c470c9.
	lineRe = regexp.MustCompile(`^v(\d+)\.(\d+)\.\d+`)
)

// lndVersion is a parsed lnd release tag.
type lndVersion struct {
	// Tag is the original tag, for example v0.20.5-beta.rc1.
	Tag string

	Major int
	Minor int
	Patch int

	// RC is the release candidate number, or zero for a final release.
	RC int
}

// parseLndTag parses an lnd release tag. It returns false for anything that
// is not a beta release or release candidate, such as the tags lnd uses for
// its submodules.
func parseLndTag(tag string) (lndVersion, bool) {
	m := lndTagRe.FindStringSubmatch(tag)
	if m == nil {
		return lndVersion{}, false
	}

	v := lndVersion{Tag: tag}
	v.Major, _ = strconv.Atoi(m[1])
	v.Minor, _ = strconv.Atoi(m[2])
	v.Patch, _ = strconv.Atoi(m[3])
	if m[4] != "" {
		v.RC, _ = strconv.Atoi(m[4])
	}

	return v, true
}

// Line returns the major release line of the version, for example v0.20.
func (v lndVersion) Line() string {
	return fmt.Sprintf("v%d.%d", v.Major, v.Minor)
}

// IsRC reports whether the version is a release candidate.
func (v lndVersion) IsRC() bool {
	return v.RC != 0
}

// Final returns the tag of the final release this version belongs to, for
// example v0.20.5-beta for v0.20.5-beta.rc1.
func (v lndVersion) Final() string {
	return fmt.Sprintf("v%d.%d.%d-beta", v.Major, v.Minor, v.Patch)
}

// Less reports whether v sorts before o. Release candidates sort before the
// final release of the same version.
func (v lndVersion) Less(o lndVersion) bool {
	switch {
	case v.Major != o.Major:
		return v.Major < o.Major

	case v.Minor != o.Minor:
		return v.Minor < o.Minor

	case v.Patch != o.Patch:
		return v.Patch < o.Patch
	}

	return rcRank(v.RC) < rcRank(o.RC)
}

// rcRank maps a release candidate number to a sort key where a final release
// (zero) sorts after every release candidate.
func rcRank(rc int) int {
	if rc == 0 {
		return int(^uint(0) >> 1)
	}

	return rc
}

// lineOf returns the major release line of an lnd module version as it
// appears in a go.mod file.
func lineOf(version string) (string, bool) {
	m := lineRe.FindStringSubmatch(version)
	if m == nil {
		return "", false
	}

	return fmt.Sprintf("v%s.%s", m[1], m[2]), true
}
