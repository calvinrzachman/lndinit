package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/mod/semver"
)

const (
	testBaseRepo  = "lightninglabs/lnd"
	testImageRepo = "lightninglabs/lndinit"
)

// fakeRegistry is an in-memory registry keyed by repo, then tag.
type fakeRegistry struct {
	tags map[string]map[string]bool

	// hidden holds tags that exist but are left out of search results,
	// like a search that quietly comes back short.
	hidden map[string]bool

	// err fails every call, existsErr only TagExists.
	err       error
	existsErr error
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{
		tags:   make(map[string]map[string]bool),
		hidden: make(map[string]bool),
	}
}

func (f *fakeRegistry) add(repo string, tags ...string) {
	if f.tags[repo] == nil {
		f.tags[repo] = make(map[string]bool)
	}
	for _, tag := range tags {
		f.tags[repo][tag] = true
	}
}

func (f *fakeRegistry) TagExists(_ context.Context, repo,
	tag string) (bool, error) {

	if f.err != nil {
		return false, f.err
	}
	if f.existsErr != nil {
		return false, f.existsErr
	}

	return f.tags[repo][tag], nil
}

// TagsContaining mimics Docker Hub's name filter, which is a substring match
// rather than a suffix match.
func (f *fakeRegistry) TagsContaining(_ context.Context, repo,
	substr string) ([]string, error) {

	if f.err != nil {
		return nil, f.err
	}

	var tags []string
	for tag := range f.tags[repo] {
		if strings.Contains(tag, substr) && !f.hidden[tag] {
			tags = append(tags, tag)
		}
	}

	return tags, nil
}

// testInputs returns plan inputs with the known lndinit release history.
func testInputs(lndTags []string, dockerTags ...string) planInputs {
	return planInputs{
		LndTags:    lndTags,
		Releases:   knownReleases,
		DockerTags: dockerTags,
	}
}

// runPlan runs plan with every lnd release published.
func runPlan(cfg planConfig, in planInputs, reg registry) ([]decision, error) {
	return plan(context.Background(), cfg, in, reg, newFakeUpstream())
}

// fakeUpstream reports every lnd release as published unless it is listed in
// unpublished. Its lndinit history is a single line of releases on main,
// ordered by version, except for tags in offMain, which sit on their own
// branch like a maintenance release.
type fakeUpstream struct {
	unpublished map[string]bool
	offMain     map[string]bool

	// db overrides the database versions of an lnd version, which are
	// otherwise the same for all.
	db map[string]dbVersions

	err error
}

func newFakeUpstream() *fakeUpstream {
	return &fakeUpstream{
		unpublished: make(map[string]bool),
		offMain:     make(map[string]bool),
		db:          make(map[string]dbVersions),
	}
}

func (f *fakeUpstream) DBVersions(_ context.Context,
	version string) (dbVersions, error) {

	if f.err != nil {
		return dbVersions{}, f.err
	}
	if v, ok := f.db[version]; ok {
		return v, nil
	}

	return dbVersions{Channel: 35, WatchtowerClient: 8}, nil
}

func (f *fakeUpstream) IsAncestor(_ context.Context, older,
	newer string) (bool, error) {

	switch {
	case f.err != nil:
		return false, f.err

	case older == newer:
		return true, nil

	case f.offMain[older] || f.offMain[newer]:
		return false, nil
	}

	return semver.Compare(older, newer) < 0, nil
}

func (f *fakeUpstream) ReleasePublished(_ context.Context,
	tag string) (bool, error) {

	if f.err != nil {
		return false, f.err
	}

	return !f.unpublished[tag], nil
}

func testConfig() planConfig {
	return planConfig{
		Lines:     []string{"v0.20", "v0.21"},
		BaseRepo:  testBaseRepo,
		ImageRepo: testImageRepo,
	}
}

// statuses flattens decisions into "lnd=status" strings for comparison.
func statuses(decisions []decision) []string {
	out := make([]string, 0, len(decisions))
	for _, d := range decisions {
		out = append(out, d.Lnd+"="+string(d.Status))
	}

	return out
}

// TestPlanStatuses walks through each outcome the reconciler can reach.
func TestPlanStatuses(t *testing.T) {
	t.Parallel()

	reg := newFakeRegistry()
	reg.add(testBaseRepo,
		"v0.20.3-beta", "v0.20.4-beta", "v0.20.5-beta.rc1",
		"v0.21.4-beta.rc1",
	)
	reg.add(testImageRepo,
		"v0.1.34-beta-lnd-v0.20.5-beta.rc1",
		"v0.1.38-beta-lnd-v0.21.4-beta.rc1",
		"daily-testing-only",
	)

	lndTags := []string{
		// Superseded by its final release.
		"v0.20.3-beta.rc1",
		// Missing lndinit image, base image published.
		"v0.20.3-beta",
		"v0.20.4-beta",
		// Already built.
		"v0.20.5-beta.rc1",
		"v0.21.4-beta.rc1",
		// Tagged but lnd's image is not published yet.
		"v0.20.6-beta.rc1",
		// The newest v0.21 lndinit is on an lnd pseudo-version.
		"v0.21.5-beta.rc1",
		// New line: in scope, but no lndinit targets it.
		"v0.22.0-beta.rc1",
		// Out of scope or not a release.
		"v0.19.3-beta",
		"kvdb/v1.4.16",
	}

	cfg := testConfig()
	cfg.Lines = append(cfg.Lines, "v0.22")

	in := testInputs(lndTags)
	decisions, err := runPlan(cfg, in, reg)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	want := []string{
		"v0.20.3-beta.rc1=superseded-rc",
		"v0.20.3-beta=build",
		"v0.20.4-beta=build",
		"v0.20.5-beta.rc1=present",
		"v0.20.6-beta.rc1=wait-base-image",
		"v0.21.4-beta.rc1=present",
		"v0.21.5-beta.rc1=needs-manual",
		"v0.22.0-beta.rc1=no-lndinit",
	}
	if got := statuses(decisions); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	build := decisions[1]
	if build.RuleLndinit != "v0.1.34-beta" ||
		build.Image != "v0.1.34-beta-lnd-v0.20.3-beta" {

		t.Fatalf("unexpected build decision: %+v", build)
	}
}

// TestPlanExistingSuffix checks that an image for a longer lnd tag is not
// mistaken for an image of a shorter one. Docker Hub's name filter matches
// substrings, so -lnd-v0.20.5-beta also matches -lnd-v0.20.5-beta.rc1.
func TestPlanExistingSuffix(t *testing.T) {
	t.Parallel()

	reg := newFakeRegistry()
	reg.add(testBaseRepo, "v0.20.5-beta")
	reg.add(testImageRepo, "v0.1.34-beta-lnd-v0.20.5-beta.rc1")

	in := testInputs([]string{"v0.20.5-beta"})
	decisions, err := runPlan(testConfig(), in, reg)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	if len(decisions) != 1 || decisions[0].Status != statusBuild {
		t.Fatalf("expected a build, got %+v", decisions)
	}
}

// TestPlanPresentWithOtherLndinit checks that an image paired with an older
// lndinit still counts as present. The reconciler fills gaps; it does not
// rebuild releases when a newer lndinit appears on the line.
func TestPlanPresentWithOtherLndinit(t *testing.T) {
	t.Parallel()

	reg := newFakeRegistry()
	reg.add(testBaseRepo, "v0.20.1-beta")
	reg.add(testImageRepo, "v0.1.33-beta-lnd-v0.20.1-beta")

	in := testInputs([]string{"v0.20.1-beta"})
	decisions, err := runPlan(testConfig(), in, reg)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	d := decisions[0]
	if d.Status != statusPresent || d.RuleLndinit != "v0.1.34-beta" ||
		!reflect.DeepEqual(d.Existing,
			[]string{"v0.1.33-beta-lnd-v0.20.1-beta"}) {

		t.Fatalf("unexpected decision: %+v", d)
	}
}

// TestPlanIncludeSupersededRCs checks the opt-in for building rcs whose
// final release is already out.
func TestPlanIncludeSupersededRCs(t *testing.T) {
	t.Parallel()

	reg := newFakeRegistry()
	reg.add(testBaseRepo, "v0.20.3-beta.rc1", "v0.20.3-beta")

	cfg := testConfig()
	cfg.IncludeSupersededRCs = true

	in := testInputs([]string{"v0.20.3-beta", "v0.20.3-beta.rc1"})
	decisions, err := runPlan(cfg, in, reg)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	want := []string{
		"v0.20.3-beta.rc1=build",
		"v0.20.3-beta=build",
	}
	if got := statuses(decisions); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestPlanRegistryError checks that a registry failure aborts the pass
// instead of being read as "image missing".
func TestPlanRegistryError(t *testing.T) {
	t.Parallel()

	reg := newFakeRegistry()
	reg.err = errors.New("rate limited")

	in := testInputs([]string{"v0.20.5-beta"})
	_, err := runPlan(testConfig(), in, reg)
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("expected registry error, got %v", err)
	}
}

// TestPlanExactLookupDisagrees checks that a build is never planned when the
// search missed an image that the exact lookup finds.
func TestPlanExactLookupDisagrees(t *testing.T) {
	t.Parallel()

	const image = "v0.1.34-beta-lnd-v0.20.5-beta"

	reg := newFakeRegistry()
	reg.add(testBaseRepo, "v0.20.5-beta")
	reg.add(testImageRepo, image)
	reg.hidden[image] = true

	in := testInputs([]string{"v0.20.5-beta"})
	_, err := runPlan(testConfig(), in, reg)
	if err == nil || !strings.Contains(err.Error(), "did not return it") {
		t.Fatalf("expected a search disagreement error, got %v", err)
	}
}

// TestPlanLookupError checks that a failed exact lookup aborts the pass.
func TestPlanLookupError(t *testing.T) {
	t.Parallel()

	reg := newFakeRegistry()
	reg.existsErr = errors.New("connection reset")

	in := testInputs([]string{"v0.20.5-beta"})
	_, err := runPlan(testConfig(), in, reg)
	if err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("expected lookup error, got %v", err)
	}
}

// TestPlanInvalidRegistryTag checks that a tag name Docker would never
// accept aborts the pass instead of reaching the report.
func TestPlanInvalidRegistryTag(t *testing.T) {
	t.Parallel()

	reg := newFakeRegistry()
	reg.add(testBaseRepo, "v0.20.5-beta")
	reg.add(testImageRepo, "x|## forged row|-lnd-v0.20.5-beta")

	in := testInputs([]string{"v0.20.5-beta"})
	_, err := runPlan(testConfig(), in, reg)
	if err == nil || !strings.Contains(err.Error(), "invalid tag") {
		t.Fatalf("expected invalid tag error, got %v", err)
	}
}

// TestPlanSupersededByLaterRC checks that an rc is superseded by a later rc
// of the same version, not only by the final release.
func TestPlanSupersededByLaterRC(t *testing.T) {
	t.Parallel()

	reg := newFakeRegistry()
	reg.add(testBaseRepo, "v0.20.6-beta.rc1", "v0.20.6-beta.rc2")

	in := testInputs([]string{"v0.20.6-beta.rc2", "v0.20.6-beta.rc1"})
	decisions, err := runPlan(testConfig(), in, reg)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	want := []string{
		"v0.20.6-beta.rc1=superseded-rc",
		"v0.20.6-beta.rc2=build",
	}
	if got := statuses(decisions); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestPlanPresentMismatch checks that an image only counts as present when
// its lndinit was built for the same lnd line.
func TestPlanPresentMismatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		existing []string
		want     status
	}{{
		// The case on Docker Hub today, before v0.1.35 was added:
		// v0.1.34 targets v0.20 but was paired with a v0.21 rc.
		name:     "wrong line only",
		existing: []string{"v0.1.34-beta-lnd-v0.21.0-beta.rc1"},
		want:     statusPresentMismatch,
	}, {
		name: "one image on the right line",
		existing: []string{
			"v0.1.34-beta-lnd-v0.21.0-beta.rc1",
			"v0.1.35-beta-lnd-v0.21.0-beta.rc1",
		},
		want: statusPresent,
	}, {
		name:     "unknown lndinit",
		existing: []string{"v0.1.99-beta-lnd-v0.21.0-beta.rc1"},
		want:     statusPresentMismatch,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reg := newFakeRegistry()
			reg.add(testBaseRepo, "v0.21.0-beta.rc1")
			reg.add(testImageRepo, tc.existing...)

			in := testInputs([]string{"v0.21.0-beta.rc1"})
			decisions, err := runPlan(testConfig(), in, reg)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			if got := decisions[0].Status; got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// TestPlanTaggedNoImage checks that a pushed docker/ tag without an image
// holds the release back instead of planning a second build.
func TestPlanTaggedNoImage(t *testing.T) {
	t.Parallel()

	reg := newFakeRegistry()
	reg.add(testBaseRepo, "v0.20.5-beta")

	in := testInputs(
		[]string{"v0.20.5-beta"},
		"docker/v0.1.34-beta-lnd-v0.20.5-beta",
		// A longer lnd tag must not match.
		"docker/v0.1.34-beta-lnd-v0.20.5-beta.rc1",
		// Neither must a tag outside docker/.
		"v0.1.34-beta-lnd-v0.20.5-beta",
	)

	decisions, err := runPlan(testConfig(), in, reg)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	d := decisions[0]
	want := []string{"docker/v0.1.34-beta-lnd-v0.20.5-beta"}
	if d.Status != statusTaggedNoImage || !reflect.DeepEqual(d.Pending,
		want) {

		t.Fatalf("unexpected decision: %+v", d)
	}

	// Once the image is published the release is simply present.
	reg.add(testImageRepo, "v0.1.34-beta-lnd-v0.20.5-beta")
	decisions, err = runPlan(testConfig(), in, reg)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if decisions[0].Status != statusPresent {
		t.Fatalf("expected present, got %+v", decisions[0])
	}
}

// TestPlanDecisionFields checks what each kind of row carries. Only a build
// row names an image, so nothing downstream can build from another row, and
// the rule's choice comes with its lnd dependency and any flags.
func TestPlanDecisionFields(t *testing.T) {
	t.Parallel()

	reg := newFakeRegistry()
	reg.add(testBaseRepo, "v0.20.6-beta")

	in := testInputs([]string{
		"v0.20.6-beta", "v0.20.7-beta.rc1", "v0.21.5-beta",
	})
	decisions, err := runPlan(testConfig(), in, reg)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	build, wait, manual := decisions[0], decisions[1], decisions[2]
	if build.Status != statusBuild ||
		build.Image != "v0.1.34-beta-lnd-v0.20.6-beta" ||
		build.RuleLndDep != "v0.20.1-beta" || build.Flags != nil {

		t.Fatalf("unexpected build row: %+v", build)
	}

	if wait.Status != statusWaitBase || wait.Image != "" ||
		wait.RuleLndinit != "v0.1.34-beta" {

		t.Fatalf("unexpected wait row: %+v", wait)
	}

	if manual.Status != statusNeedsManual || manual.Image != "" ||
		manual.RuleLndinit != "v0.1.38-beta" ||
		!reflect.DeepEqual(manual.Flags,
			[]string{"pseudo-version-dep"}) {

		t.Fatalf("unexpected manual row: %+v", manual)
	}
}

// TestPlanMaintenanceRelease checks that a maintenance release built on a
// tagged lnd, on its own branch, is automated for its line even though newer
// pseudo-version releases exist on main.
func TestPlanMaintenanceRelease(t *testing.T) {
	t.Parallel()

	reg := newFakeRegistry()
	reg.add(testBaseRepo, "v0.21.5-beta")

	in := testInputs([]string{"v0.21.5-beta"})
	in.Releases = append(append([]lndinitRelease{}, in.Releases...),
		lndinitRelease{Tag: "v0.1.39-beta", LndDep: "v0.21.0-beta"},
		lndinitRelease{
			Tag: "v0.1.40-beta",
			LndDep: "v0.21.0-beta.rc2.0.20260717013942-" +
				"f02cf4c470c9",
		},
	)

	up := newFakeUpstream()
	up.offMain["v0.1.39-beta"] = true

	decisions, err := plan(context.Background(), testConfig(), in, reg, up)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	d := decisions[0]
	if d.Status != statusBuild ||
		d.Image != "v0.1.39-beta-lnd-v0.21.5-beta" {

		t.Fatalf("unexpected decision: %+v", d)
	}
}

// TestPlanWaitSignatures checks that a release whose GitHub release is still
// a draft waits, even though its tag and image already exist, and that a
// failed lookup aborts the pass.
func TestPlanWaitSignatures(t *testing.T) {
	t.Parallel()

	reg := newFakeRegistry()
	reg.add(testBaseRepo, "v0.20.6-beta")

	up := newFakeUpstream()
	up.unpublished["v0.20.6-beta"] = true

	in := testInputs([]string{"v0.20.6-beta"})
	decisions, err := plan(context.Background(), testConfig(), in, reg, up)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	d := decisions[0]
	if d.Status != statusWaitSignatures || d.Image != "" {
		t.Fatalf("unexpected decision: %+v", d)
	}

	up.err = errors.New("rate limited")
	_, err = plan(context.Background(), testConfig(), in, reg, up)
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("expected lookup error, got %v", err)
	}
}

// TestPlanDBMismatch checks that a pairing whose database versions differ is
// held back with an explanation, and that a failed lookup aborts the pass.
func TestPlanDBMismatch(t *testing.T) {
	t.Parallel()

	reg := newFakeRegistry()
	reg.add(testBaseRepo, "v0.20.6-beta")

	// A patch release that added a channeldb migration.
	up := newFakeUpstream()
	up.db["v0.20.6-beta"] = dbVersions{Channel: 36, WatchtowerClient: 8}

	in := testInputs([]string{"v0.20.6-beta"})
	decisions, err := plan(context.Background(), testConfig(), in, reg, up)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	d := decisions[0]
	if d.Status != statusDBMismatch || d.Image != "" ||
		!strings.Contains(d.Note, "channeldb 36") {

		t.Fatalf("unexpected decision: %+v", d)
	}
}
