package main

import (
	"context"
	"regexp"
	"strings"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

// lndinitTagRe matches an lndinit version tag such as v0.1.34-beta. Docker
// build tags (docker/...) and anything else are ignored.
var lndinitTagRe = regexp.MustCompile(`^v\d+\.\d+\.\d+-beta$`)

// lndinitRelease is an lndinit version tag together with the lnd module
// version its go.mod requires at that tag.
type lndinitRelease struct {
	// Tag is the lndinit version tag, for example v0.1.34-beta.
	Tag string

	// LndDep is the github.com/lightningnetwork/lnd version required by
	// go.mod at Tag, for example v0.20.1-beta.
	LndDep string
}

// pickLndinit returns the newest lndinit release that was compiled against
// the given lnd major line.
//
// The binary inside an lndinit image comes from an lndinit version tag, and
// that binary embeds lnd code at the version its go.mod requires. Within an
// lnd line the parts lndinit depends on stay compatible, but across lines
// they do not: migrate-db, for one, refuses to run unless the database
// version matches the one it was compiled with, and lnd only bumps those
// versions at major releases. So an lnd release is paired with the newest
// lndinit built for the same line, not with the newest lndinit overall.
//
// The second return value is false if no lndinit release targets the line,
// which means a new lndinit binary release is needed first.
func pickLndinit(releases []lndinitRelease, line string) (string, bool) {
	var best string
	for _, r := range releases {
		if !lndinitTagRe.MatchString(r.Tag) {
			continue
		}

		depLine, ok := lineOf(r.LndDep)
		if !ok || depLine != line {
			continue
		}

		if best == "" || semver.Compare(r.Tag, best) > 0 {
			best = r.Tag
		}
	}

	return best, best != ""
}

// releaseLine returns the lnd major line the given lndinit release was
// compiled against. The second return value is false for a tag that is not
// a known lndinit release.
func releaseLine(releases []lndinitRelease, tag string) (string, bool) {
	r, ok := findRelease(releases, tag)
	if !ok {
		return "", false
	}

	return lineOf(r.LndDep)
}

// findRelease returns the lndinit release with the given tag.
func findRelease(releases []lndinitRelease,
	tag string) (lndinitRelease, bool) {

	for _, r := range releases {
		if r.Tag == tag {
			return r, true
		}
	}

	return lndinitRelease{}, false
}

// depFlags notes how an lndinit release's lnd dependency differs from a final
// lnd release, since the pairing rule only reads the major line from it.
//
// A pseudo-version names a commit, and its line comes from the nearest tag
// below that commit. lndinit v0.1.37-beta and later depend on such a commit
// on lnd's master branch, which is classified as v0.21 but is not part of
// any v0.21 release. The pairing is correct only as long as that commit
// agrees with the line on the things lndinit relies on, such as database
// versions, and the rule can't check that. A release candidate dependency
// is flagged for the same reason, to a lesser degree.
func depFlags(dep string) []string {
	switch {
	case module.IsPseudoVersion(dep):
		return []string{"pseudo-version-dep"}

	case strings.Contains(dep, ".rc"):
		return []string{"rc-dep"}
	}

	return nil
}

// pseudoDep reports whether the release depends on an lnd pseudo-version, such
// as a commit on lnd's master branch, rather than a tagged lnd release.
func (r lndinitRelease) pseudoDep() bool {
	return module.IsPseudoVersion(r.LndDep)
}

// ancestorFunc reports whether lndinit tag older is an ancestor of tag newer.
type ancestorFunc func(ctx context.Context, older, newer string) (bool,
	error)

// chooseLndinit picks the lndinit release to automate for an lnd line.
//
// Only releases built on a tagged lnd are automated, since for those go.mod
// says exactly which line the lnd code belongs to. A pseudo-version only names
// the tag below some commit, which for lndinit v0.1.37-beta and later is a
// commit on lnd's master branch.
//
// The newest tagged-dependency release is used unless a newer
// pseudo-version release has replaced it on the same branch, as v0.1.38-beta
// replaced v0.1.36-beta. Automating the older release then would quietly ship
// images without the later changes, so the line needs a manual build
// instead, and manual is true. A maintenance release cut on its own branch,
// such as a v0.21 release from v0.1.36-beta, is not an ancestor of the
// pseudo-version releases and is still used.
//
// The returned release is the tagged one to automate, or the newest
// pseudo-version release if the line needs a manual build. found is false if
// no lndinit release targets the line.
func chooseLndinit(ctx context.Context, releases []lndinitRelease,
	line string, isAncestor ancestorFunc) (rel lndinitRelease, manual,
	found bool, err error) {

	var tagged, pseudo []lndinitRelease
	for _, r := range releases {
		if r.pseudoDep() {
			pseudo = append(pseudo, r)
		} else {
			tagged = append(tagged, r)
		}
	}

	newestPseudo, hasPseudo := pickLndinit(pseudo, line)
	newestTagged, hasTagged := pickLndinit(tagged, line)

	switch {
	case !hasTagged && !hasPseudo:
		return lndinitRelease{}, false, false, nil

	case !hasTagged:
		rel, _ := findRelease(releases, newestPseudo)
		return rel, true, true, nil
	}

	for _, p := range pseudo {
		depLine, ok := lineOf(p.LndDep)
		if !ok || depLine != line ||
			semver.Compare(p.Tag, newestTagged) <= 0 {

			continue
		}

		replaced, err := isAncestor(ctx, newestTagged, p.Tag)
		if err != nil {
			return lndinitRelease{}, false, false, err
		}
		if replaced {
			rel, _ := findRelease(releases, newestPseudo)
			return rel, true, true, nil
		}
	}

	rel, _ = findRelease(releases, newestTagged)
	return rel, false, true, nil
}
