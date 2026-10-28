package main

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// dockerTagRe is the grammar Docker accepts for an image tag.
var dockerTagRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

// status is the outcome the reconciler reached for one lnd release.
type status string

const (
	// statusPresent means an lndinit image already exists for the lnd
	// release, built with an lndinit for the same lnd line. It need not
	// be the lndinit the rule would pick today: the reconciler fills
	// gaps and does not rebuild when a newer lndinit ships.
	statusPresent status = "present"

	// statusPresentMismatch means lndinit images exist for the lnd
	// release, but every one of them was built with an lndinit for
	// another lnd line, or with one the reconciler doesn't know. It is
	// reported for a human to look at. Nothing is rebuilt automatically,
	// since replacing a published image is a decision.
	statusPresentMismatch status = "present-mismatch"

	// statusBuild means every precondition holds and an image should be
	// built.
	statusBuild status = "build"

	// statusWaitSignatures means lnd's GitHub release for the tag isn't
	// published yet. lnd publishes once enough developers have signed the
	// release and its own check of the image has passed, so until then the
	// base image isn't known to match the signed release. The next pass
	// will pick it up.
	statusWaitSignatures status = "wait-signatures"

	// statusWaitBase means the lnd image the build would start from has
	// not been published yet. The next pass will pick it up.
	statusWaitBase status = "wait-base-image"

	// statusTaggedNoImage means a docker/ tag has been pushed for the lnd
	// release but no image exists yet. Its build is still running or has
	// failed. Planning another build would race it, possibly with a
	// different lndinit, so a person or the next pass has to look first.
	statusTaggedNoImage status = "tagged-no-image"

	// statusDBMismatch means the database versions compiled into the
	// lndinit's lnd code differ from those of the lnd release, so
	// migrate-db in the image would refuse to run against that lnd's
	// databases. lnd normally changes them only at major releases, so this
	// most likely means a patch release with a migration, which needs an
	// lndinit release for the line.
	statusDBMismatch status = "db-mismatch"

	// statusNeedsManual means the newest lndinit for the line is built on
	// an lnd pseudo-version, so go.mod doesn't say which line its lnd code
	// belongs to, and the image has to be built by hand. See
	// chooseLndinit.
	statusNeedsManual status = "needs-manual"

	// statusNoLndinit means no lndinit release targets the lnd line yet.
	// A new lndinit binary release is required, so we fail closed.
	statusNoLndinit status = "no-lndinit"

	// statusSuperseded means the release is an rc and a later rc or the
	// final release of the same version is already tagged, so building an
	// image for it is not useful.
	statusSuperseded status = "superseded-rc"
)

// registry answers the questions the reconciler asks about published images.
type registry interface {
	// TagExists reports whether repo:tag exists.
	TagExists(ctx context.Context, repo, tag string) (bool, error)

	// TagsContaining returns every tag in repo whose name contains
	// substr. It is a search, so callers must check for an exact match
	// themselves.
	TagsContaining(ctx context.Context, repo,
		substr string) ([]string, error)
}

// upstream answers questions about lnd releases and lndinit's history that
// the planner asks for individual releases.
type upstream interface {
	// ReleasePublished reports whether lnd's GitHub release for tag has
	// been published.
	ReleasePublished(ctx context.Context, tag string) (bool, error)

	// IsAncestor reports whether lndinit tag older is an ancestor of tag
	// newer.
	IsAncestor(ctx context.Context, older, newer string) (bool, error)

	// DBVersions returns the database versions compiled into lnd at
	// version, a tag or pseudo-version.
	DBVersions(ctx context.Context, version string) (dbVersions, error)
}

// planConfig holds the inputs that shape a reconcile pass.
type planConfig struct {
	// Lines are the lnd major lines to reconcile, for example v0.20.
	Lines []string

	// BaseRepo is the image repository lnd images are published to.
	BaseRepo string

	// ImageRepo is the image repository lndinit images are published to.
	ImageRepo string

	// IncludeSupersededRCs builds images for rcs that a later rc or the
	// final release has superseded, instead of skipping them.
	IncludeSupersededRCs bool
}

// planInputs is the tag data a reconcile pass works from.
type planInputs struct {
	// LndTags are lnd's tags, releases and anything else.
	LndTags []string

	// Releases are the lndinit version tags with their lnd dependency.
	Releases []lndinitRelease

	// DockerTags are the docker/<lndinit>-lnd-<lnd> git tags pushed to
	// trigger image builds.
	DockerTags []string
}

// decision is the reconciler's conclusion for one lnd release.
type decision struct {
	// Lnd is the lnd release tag.
	Lnd string `json:"lnd"`

	// Line is the lnd major line of the release.
	Line string `json:"line"`

	// Status is the outcome for the release.
	Status status `json:"status"`

	// RuleLndinit is the lndinit version the pairing rule picks for the
	// release today. On a present row it may differ from the lndinit in
	// the existing image, which Existing lists.
	RuleLndinit string `json:"rule_lndinit,omitempty"`

	// RuleLndDep is the lnd version RuleLndinit was compiled against.
	RuleLndDep string `json:"rule_lnd_dep,omitempty"`

	// Flags notes anything about the pairing a person should check, see
	// depFlags.
	Flags []string `json:"flags,omitempty"`

	// Image is the lndinit image tag to build. It is only set when Status
	// is build, so a consumer can't build from any other row by mistake.
	Image string `json:"image,omitempty"`

	// Existing lists the lndinit image tags already published for the
	// release, which may pair it with a different lndinit version.
	Existing []string `json:"existing,omitempty"`

	// Pending lists docker/ git tags pushed for the release whose image
	// does not exist yet.
	Pending []string `json:"pending,omitempty"`

	// Note explains a db-mismatch decision.
	Note string `json:"note,omitempty"`
}

// imageTag returns the lndinit image tag for a pairing, in the format the
// docker workflow uses: <lndinit>-lnd-<lnd>.
func imageTag(lndinit, lnd string) string {
	return fmt.Sprintf("%s-lnd-%s", lndinit, lnd)
}

// plan decides, for every lnd release on the configured lines, whether an
// lndinit image needs to be built.
//
// An image is due only when a lndinit release targets the lnd line, lnd's
// release is published, the lnd base image exists and no lndinit image exists
// for the release yet. The
// pass holds no state of its own. It recomputes everything from tags, so a
// missed or failed pass is corrected by the next one.
func plan(ctx context.Context, cfg planConfig, in planInputs,
	reg registry, up upstream) ([]decision, error) {

	lines := make(map[string]bool, len(cfg.Lines))
	for _, line := range cfg.Lines {
		lines[line] = true
	}

	// newest maps each version (by its final tag) to the rank of the
	// latest tag seen for it, where a final release outranks every rc.
	var (
		versions []lndVersion
		newest   = make(map[string]int)
		seen     = make(map[string]bool)
	)
	for _, tag := range in.LndTags {
		v, ok := parseLndTag(tag)
		if !ok || !lines[v.Line()] || seen[tag] {
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
		d, err := decide(ctx, cfg, in, v, newest, reg, up)
		if err != nil {
			return nil, err
		}

		decisions = append(decisions, d)
	}

	return decisions, nil
}

// decide reaches the decision for a single lnd release.
func decide(ctx context.Context, cfg planConfig, in planInputs,
	v lndVersion, newest map[string]int, reg registry,
	up upstream) (decision, error) {

	d := decision{Lnd: v.Tag, Line: v.Line()}

	superseded := v.IsRC() && newest[v.Final()] > rcRank(v.RC)
	if superseded && !cfg.IncludeSupersededRCs {
		d.Status = statusSuperseded
		return d, nil
	}

	// The search also matches longer lnd tags (-lnd-v0.20.5-beta matches
	// -lnd-v0.20.5-beta.rc1), so only exact suffix matches count.
	suffix := "-lnd-" + v.Tag
	tags, err := reg.TagsContaining(ctx, cfg.ImageRepo, suffix)
	if err != nil {
		return d, fmt.Errorf("unable to list %s images for %s: %w",
			cfg.ImageRepo, v.Tag, err)
	}
	for _, tag := range tags {
		// Tag names end up in the report and the step summary, so a
		// name Docker itself would never accept means the registry
		// answer can't be trusted.
		if !dockerTagRe.MatchString(tag) {
			return d, fmt.Errorf("%s returned invalid tag %q",
				cfg.ImageRepo, tag)
		}

		if strings.HasSuffix(tag, suffix) {
			d.Existing = append(d.Existing, tag)
		}
	}
	sort.Strings(d.Existing)

	rel, manual, found, err := chooseLndinit(
		ctx, in.Releases, v.Line(), up.IsAncestor,
	)
	if err != nil {
		return d, fmt.Errorf("choosing lndinit for %s: %w", v.Tag, err)
	}
	if found {
		d.RuleLndinit = rel.Tag
		d.RuleLndDep = rel.LndDep
		d.Flags = depFlags(rel.LndDep)
	}

	if len(d.Existing) > 0 {
		d.Status = statusPresentMismatch
		for _, tag := range d.Existing {
			lndinit := strings.TrimSuffix(tag, suffix)
			line, ok := releaseLine(in.Releases, lndinit)
			if ok && line == v.Line() {
				d.Status = statusPresent
				break
			}
		}

		return d, nil
	}

	for _, tag := range in.DockerTags {
		if strings.HasPrefix(tag, "docker/") &&
			strings.HasSuffix(tag, suffix) {

			d.Pending = append(d.Pending, tag)
		}
	}
	if len(d.Pending) > 0 {
		sort.Strings(d.Pending)
		d.Status = statusTaggedNoImage
		return d, nil
	}

	if !found {
		d.Status = statusNoLndinit
		return d, nil
	}
	if manual {
		d.Status = statusNeedsManual
		return d, nil
	}

	image := imageTag(d.RuleLndinit, v.Tag)
	if !dockerTagRe.MatchString(image) {
		return d, fmt.Errorf("image tag %q is not a valid Docker tag",
			image)
	}

	published, err := up.ReleasePublished(ctx, v.Tag)
	if err != nil {
		return d, fmt.Errorf("unable to look up the lnd release for "+
			"%s: %w", v.Tag, err)
	}
	if !published {
		d.Status = statusWaitSignatures
		return d, nil
	}

	baseExists, err := reg.TagExists(ctx, cfg.BaseRepo, v.Tag)
	if err != nil {
		return d, fmt.Errorf("unable to look up %s:%s: %w",
			cfg.BaseRepo, v.Tag, err)
	}
	if !baseExists {
		d.Status = statusWaitBase
		return d, nil
	}

	want, err := up.DBVersions(ctx, v.Tag)
	if err != nil {
		return d, fmt.Errorf("reading database versions of lnd %s: %w",
			v.Tag, err)
	}
	have, err := up.DBVersions(ctx, rel.LndDep)
	if err != nil {
		return d, fmt.Errorf("reading database versions of lnd %s: %w",
			rel.LndDep, err)
	}
	if have != want {
		d.Status = statusDBMismatch
		d.Note = fmt.Sprintf("%s has %s, lnd %s has %s", rel.Tag,
			have, v.Tag, want)
		return d, nil
	}

	// Everything so far says the image is missing, but the existing
	// images came from a search, and a search that quietly came back
	// short would look just like a missing image. Look the exact tag up
	// before asking for a build, and treat a disagreement as an error.
	imageExists, err := reg.TagExists(ctx, cfg.ImageRepo, image)
	if err != nil {
		return d, fmt.Errorf("unable to look up %s:%s: %w",
			cfg.ImageRepo, image, err)
	}
	if imageExists {
		return d, fmt.Errorf("%s:%s exists but searching %s for "+
			"%q did not return it", cfg.ImageRepo, image,
			cfg.ImageRepo, suffix)
	}

	d.Image = image
	d.Status = statusBuild
	return d, nil
}
