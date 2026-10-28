package main

import (
	"context"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

// lndinitRelease is an lndinit version tag together with the lnd module
// version its go.mod requires at that tag.
type lndinitRelease struct {
	// Tag is the lndinit version tag, for example v0.1.34-beta.
	Tag string

	// LndDep is the github.com/lightningnetwork/lnd version required by
	// go.mod at Tag, for example v0.20.1-beta.
	LndDep string
}

// ancestorFunc reports whether lndinit tag older is an ancestor of tag newer.
type ancestorFunc func(ctx context.Context, older, newer string) (bool,
	error)

// newestFor returns the newest release whose lnd dependency is on the given
// lnd major line.
func newestFor(releases []lndinitRelease, line string) (lndinitRelease,
	bool) {

	var best lndinitRelease
	for _, r := range releases {
		depLine, ok := lineOf(r.LndDep)
		if !ok || depLine != line {
			continue
		}

		if best.Tag == "" || semver.Compare(r.Tag, best.Tag) > 0 {
			best = r
		}
	}

	return best, best.Tag != ""
}

// chooseLndinit picks the lndinit release to build images with for an lnd
// major line.
//
// The binary in an lndinit image embeds lnd code at the version lndinit's
// go.mod requires, and it has to match the lnd it runs next to: migrate-db
// refuses a database whose version differs from the one it was compiled with,
// and lnd changes those versions at major releases. So an lnd release is
// paired with the newest lndinit built for the same line, not with the newest
// lndinit overall.
//
// Only releases built on a tagged lnd are used, since for those go.mod says
// exactly which line the lnd code belongs to. A pseudo-version only names the
// tag below some commit, which for lndinit v0.1.37-beta and later is a commit
// on lnd's master branch. If a newer pseudo-version release has replaced the
// newest tagged one on the same branch, as v0.1.38-beta replaced v0.1.36-beta,
// building with the tagged one would quietly drop the later changes, so
// manual is true and the newest pseudo-version release is returned for a
// person to build with. A maintenance release cut on its own branch is not
// an ancestor of the pseudo-version releases and is still used.
//
// found is false if no lndinit release targets the line.
func chooseLndinit(ctx context.Context, releases []lndinitRelease,
	line string, isAncestor ancestorFunc) (rel lndinitRelease, manual,
	found bool, err error) {

	var tagged, pseudo []lndinitRelease
	for _, r := range releases {
		if module.IsPseudoVersion(r.LndDep) {
			pseudo = append(pseudo, r)
		} else {
			tagged = append(tagged, r)
		}
	}

	newestTagged, hasTagged := newestFor(tagged, line)
	newestPseudo, hasPseudo := newestFor(pseudo, line)

	switch {
	case !hasTagged && !hasPseudo:
		return lndinitRelease{}, false, false, nil

	case !hasTagged:
		return newestPseudo, true, true, nil
	}

	for _, p := range pseudo {
		depLine, ok := lineOf(p.LndDep)
		if !ok || depLine != line ||
			semver.Compare(p.Tag, newestTagged.Tag) <= 0 {

			continue
		}

		replaced, err := isAncestor(ctx, newestTagged.Tag, p.Tag)
		if err != nil {
			return lndinitRelease{}, false, false, err
		}
		if replaced {
			return newestPseudo, true, true, nil
		}
	}

	return newestTagged, false, true, nil
}
