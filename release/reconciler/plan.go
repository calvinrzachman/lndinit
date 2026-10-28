package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

const (
	// baseRepo is the Docker Hub repository of the lnd image lndinit
	// images are built on.
	baseRepo = "lightninglabs/lnd"

	// imageRepo is the Docker Hub repository of the lndinit image.
	imageRepo = "lightninglabs/lndinit"
)

// status is the outcome the reconciler reached for one lnd release.
type status string

const (
	// statusBuild means every precondition holds and an image should be
	// built.
	statusBuild status = "build"

	// statusPresent means an lndinit image already exists for the lnd
	// release. The reconciler fills gaps and never rebuilds.
	statusPresent status = "present"

	// statusWaitSignatures means lnd's GitHub release isn't published
	// yet. lnd publishes once enough developers have signed the release,
	// so until then the base image isn't known to match a signed release.
	statusWaitSignatures status = "wait-signatures"

	// statusWaitBase means the lnd image the build would start from isn't
	// on Docker Hub yet.
	statusWaitBase status = "wait-base-image"

	// statusNeedsManual means the newest lndinit for the line is built on
	// an lnd pseudo-version, so the image has to be built by hand. See
	// chooseLndinit.
	statusNeedsManual status = "needs-manual"

	// statusNoLndinit means no lndinit release targets the lnd line yet,
	// so a new lndinit release is needed first.
	statusNoLndinit status = "no-lndinit"

	// statusSuperseded means the release is an rc and a later rc or the
	// final release of the same version is already tagged.
	statusSuperseded status = "superseded-rc"
)

// statusOrder is the order statuses are counted in.
var statusOrder = []status{
	statusBuild, statusWaitSignatures, statusWaitBase, statusNeedsManual,
	statusNoLndinit, statusPresent, statusSuperseded,
}

// upstream answers the questions the reconciler asks about published images,
// lnd releases and lndinit's history.
type upstream interface {
	// TagExists reports whether repo:tag exists on Docker Hub.
	TagExists(ctx context.Context, repo, tag string) (bool, error)

	// TagsContaining returns every tag in repo whose name contains
	// substr.
	TagsContaining(ctx context.Context, repo,
		substr string) ([]string, error)

	// ReleasePublished reports whether lnd's GitHub release for tag has
	// been published.
	ReleasePublished(ctx context.Context, tag string) (bool, error)

	// IsAncestor reports whether lndinit tag older is an ancestor of tag
	// newer.
	IsAncestor(ctx context.Context, older, newer string) (bool, error)
}

// decision is the reconciler's conclusion for one lnd release.
type decision struct {
	// Lnd is the lnd release tag.
	Lnd string

	// Line is the lnd major line of the release.
	Line string

	// Status is the outcome for the release.
	Status status

	// Lndinit is the lndinit release chosen for the line. It is empty
	// when no choice was needed.
	Lndinit string

	// Detail is the image to build, the images that exist, or why nothing
	// is built.
	Detail string
}

// imageTag returns the lndinit image tag for a pairing, in the format the
// docker workflow uses: <lndinit>-lnd-<lnd>.
func imageTag(lndinit, lnd string) string {
	return fmt.Sprintf("%s-lnd-%s", lndinit, lnd)
}

// plan decides, for every lnd release on the given lines, whether an lndinit
// image needs to be built. It holds no state of its own and recomputes
// everything on each pass, so a missed or failed pass is corrected by the
// next one.
func plan(ctx context.Context, lines, lndTags []string,
	releases []lndinitRelease, up upstream) ([]decision, error) {

	wanted := make(map[string]bool, len(lines))
	for _, line := range lines {
		wanted[line] = true
	}

	// newest maps each version, by its final tag, to the rank of the
	// latest tag seen for it, where a final release outranks every rc.
	var (
		versions []lndVersion
		newest   = make(map[string]int)
		seen     = make(map[string]bool)
	)
	for _, tag := range lndTags {
		v, ok := parseLndTag(tag)
		if !ok || !wanted[v.Line()] || seen[tag] {
			continue
		}
		seen[tag] = true

		versions = append(versions, v)
		if rank := rcRank(v.RC); rank > newest[v.Final()] {
			newest[v.Final()] = rank
		}
	}
	sort.Slice(versions, func(i, j int) bool {
		return versions[i].Less(versions[j])
	})

	decisions := make([]decision, 0, len(versions))
	for _, v := range versions {
		d, err := decide(ctx, v, newest, releases, up)
		if err != nil {
			return nil, err
		}

		decisions = append(decisions, d)
	}

	return decisions, nil
}

// decide reaches the decision for a single lnd release. Any error from an
// upstream lookup stops the pass, so an unclear answer is never read as
// "image missing".
func decide(ctx context.Context, v lndVersion, newest map[string]int,
	releases []lndinitRelease, up upstream) (decision, error) {

	d := decision{Lnd: v.Tag, Line: v.Line()}

	if v.IsRC() && newest[v.Final()] > rcRank(v.RC) {
		d.Status = statusSuperseded
		d.Detail = "a later rc or the final release is tagged"
		return d, nil
	}

	// The search also returns longer lnd tags, such as the rcs of a final
	// release, so only exact suffixes count.
	suffix := "-lnd-" + v.Tag
	tags, err := up.TagsContaining(ctx, imageRepo, suffix)
	if err != nil {
		return d, fmt.Errorf("listing images for %s: %w", v.Tag, err)
	}
	var existing []string
	for _, tag := range tags {
		if strings.HasSuffix(tag, suffix) {
			existing = append(existing, tag)
		}
	}
	if len(existing) > 0 {
		sort.Strings(existing)
		d.Status = statusPresent
		d.Detail = strings.Join(existing, ", ")
		return d, nil
	}

	rel, manual, found, err := chooseLndinit(
		ctx, releases, v.Line(), up.IsAncestor,
	)
	switch {
	case err != nil:
		return d, fmt.Errorf("choosing lndinit for %s: %w", v.Tag, err)

	case !found:
		d.Status = statusNoLndinit
		d.Detail = "needs an lndinit release for " + v.Line()
		return d, nil

	case manual:
		d.Status = statusNeedsManual
		d.Lndinit = rel.Tag
		d.Detail = "built on an untagged lnd, build by hand"
		return d, nil
	}
	d.Lndinit = rel.Tag

	published, err := up.ReleasePublished(ctx, v.Tag)
	if err != nil {
		return d, fmt.Errorf("looking up the lnd release %s: %w",
			v.Tag, err)
	}
	if !published {
		d.Status = statusWaitSignatures
		d.Detail = "lnd release not published yet"
		return d, nil
	}

	baseExists, err := up.TagExists(ctx, baseRepo, v.Tag)
	if err != nil {
		return d, fmt.Errorf("looking up %s:%s: %w", baseRepo, v.Tag,
			err)
	}
	if !baseExists {
		d.Status = statusWaitBase
		d.Detail = "waiting for " + baseRepo + ":" + v.Tag
		return d, nil
	}

	// The search said no image exists, but a search that came back short
	// would look the same. Look the exact tag up before asking for a
	// build, and treat a disagreement as an error.
	image := imageTag(rel.Tag, v.Tag)
	imageExists, err := up.TagExists(ctx, imageRepo, image)
	if err != nil {
		return d, fmt.Errorf("looking up %s:%s: %w", imageRepo, image,
			err)
	}
	if imageExists {
		return d, fmt.Errorf("%s:%s exists but searching for %q did "+
			"not return it", imageRepo, image, suffix)
	}

	d.Status = statusBuild
	d.Detail = image
	return d, nil
}
