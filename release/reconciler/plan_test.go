package main

import (
	"context"
	"strings"
	"testing"
)

// fakeUpstream answers upstream questions from in-memory data.
type fakeUpstream struct {
	// images holds the published images as repo:tag.
	images map[string]bool

	// hidden holds images the tag search leaves out.
	hidden map[string]bool

	// published holds the lnd tags whose GitHub release is published.
	published map[string]bool

	// ancestors holds older..newer pairs of lndinit tags.
	ancestors map[string]bool
}

func (f *fakeUpstream) TagExists(_ context.Context, repo,
	tag string) (bool, error) {

	return f.images[repo+":"+tag], nil
}

func (f *fakeUpstream) TagsContaining(_ context.Context, repo,
	substr string) ([]string, error) {

	var tags []string
	for image := range f.images {
		r, tag, _ := strings.Cut(image, ":")
		if r == repo && strings.Contains(tag, substr) &&
			!f.hidden[image] {

			tags = append(tags, tag)
		}
	}

	return tags, nil
}

func (f *fakeUpstream) ReleasePublished(_ context.Context,
	tag string) (bool, error) {

	return f.published[tag], nil
}

func (f *fakeUpstream) IsAncestor(_ context.Context, older,
	newer string) (bool, error) {

	return f.ancestors[older+".."+newer], nil
}

// testReleases mirrors lndinit's real history: v0.1.34-beta is the last
// release for v0.20, v0.1.36-beta is built on tagged lnd v0.21, and
// v0.1.38-beta replaced it on main with an lnd master commit.
var testReleases = []lndinitRelease{
	{Tag: "v0.1.34-beta", LndDep: "v0.20.1-beta"},
	{Tag: "v0.1.36-beta", LndDep: "v0.21.0-beta"},
	{
		Tag:    "v0.1.38-beta",
		LndDep: "v0.21.0-beta.rc2.0.20260717013942-f02cf4c470c9",
	},
}

// readyUpstream returns an upstream where the given lnd tags are published
// and have a base image, and v0.1.38-beta descends from v0.1.36-beta.
func readyUpstream(lndTags ...string) *fakeUpstream {
	f := &fakeUpstream{
		images:    make(map[string]bool),
		hidden:    make(map[string]bool),
		published: make(map[string]bool),
		ancestors: map[string]bool{"v0.1.36-beta..v0.1.38-beta": true},
	}
	for _, tag := range lndTags {
		f.published[tag] = true
		f.images[baseRepo+":"+tag] = true
	}

	return f
}

// TestPlan checks the decision for each kind of lnd release.
func TestPlan(t *testing.T) {
	tests := []struct {
		name     string
		lines    []string
		lndTags  []string
		releases []lndinitRelease
		setup    func(f *fakeUpstream)

		// want maps lnd tags to "status lndinit detail", with empty
		// parts left out.
		want map[string]string
	}{{
		name:    "missing image is built with the line's lndinit",
		lndTags: []string{"v0.20.6-beta"},
		want: map[string]string{
			"v0.20.6-beta": "build v0.1.34-beta " +
				"v0.1.34-beta-lnd-v0.20.6-beta",
		},
	}, {
		name:    "existing image is left alone",
		lndTags: []string{"v0.20.6-beta"},
		setup: func(f *fakeUpstream) {
			f.images[imageRepo+":v0.1.34-beta-lnd-v0.20.6-beta"] =
				true
		},
		want: map[string]string{
			"v0.20.6-beta": "present v0.1.34-beta-lnd-v0.20.6-beta",
		},
	}, {
		name:    "an rc's image does not count for the final release",
		lndTags: []string{"v0.20.6-beta"},
		setup: func(f *fakeUpstream) {
			f.images[imageRepo+":v0.1.34-beta-lnd-v0.20.6-beta.rc1"] =
				true
		},
		want: map[string]string{
			"v0.20.6-beta": "build v0.1.34-beta " +
				"v0.1.34-beta-lnd-v0.20.6-beta",
		},
	}, {
		name:    "unpublished release waits for signatures",
		lndTags: []string{"v0.20.6-beta"},
		setup: func(f *fakeUpstream) {
			delete(f.published, "v0.20.6-beta")
		},
		want: map[string]string{
			"v0.20.6-beta": "wait-signatures v0.1.34-beta lnd " +
				"release not published yet",
		},
	}, {
		name:    "missing base image waits",
		lndTags: []string{"v0.20.6-beta"},
		setup: func(f *fakeUpstream) {
			delete(f.images, baseRepo+":v0.20.6-beta")
		},
		want: map[string]string{
			"v0.20.6-beta": "wait-base-image v0.1.34-beta " +
				"waiting for lightninglabs/lnd:v0.20.6-beta",
		},
	}, {
		name:    "lndinit replaced by a pseudo-version needs a person",
		lndTags: []string{"v0.21.5-beta"},
		want: map[string]string{
			"v0.21.5-beta": "needs-manual v0.1.38-beta built on " +
				"an untagged lnd, build by hand",
		},
	}, {
		name:    "maintenance release on its own branch is used",
		lndTags: []string{"v0.21.5-beta"},
		releases: append(testReleases[:3:3], lndinitRelease{
			Tag: "v0.1.39-beta", LndDep: "v0.21.0-beta",
		}),
		want: map[string]string{
			"v0.21.5-beta": "build v0.1.39-beta " +
				"v0.1.39-beta-lnd-v0.21.5-beta",
		},
	}, {
		name:    "line without an lndinit release",
		lines:   []string{"v0.22"},
		lndTags: []string{"v0.22.0-beta"},
		want: map[string]string{
			"v0.22.0-beta": "no-lndinit needs an lndinit " +
				"release for v0.22",
		},
	}, {
		name:    "superseded rc is skipped",
		lndTags: []string{"v0.20.6-beta.rc1", "v0.20.6-beta.rc2"},
		want: map[string]string{
			"v0.20.6-beta.rc1": "superseded-rc a later rc or the " +
				"final release is tagged",
			"v0.20.6-beta.rc2": "build v0.1.34-beta " +
				"v0.1.34-beta-lnd-v0.20.6-beta.rc2",
		},
	}, {
		name: "other lines and non-release tags are ignored",
		lndTags: []string{
			"v0.19.3-beta", "v0.20.6-beta", "v0.20.6",
			"v0.20.06-beta", "kvdb/v1.4.16",
		},
		want: map[string]string{
			"v0.20.6-beta": "build v0.1.34-beta " +
				"v0.1.34-beta-lnd-v0.20.6-beta",
		},
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lines := test.lines
			if lines == nil {
				lines = []string{"v0.20", "v0.21"}
			}
			releases := test.releases
			if releases == nil {
				releases = testReleases
			}

			up := readyUpstream(test.lndTags...)
			if test.setup != nil {
				test.setup(up)
			}

			decisions, err := plan(
				context.Background(), lines, test.lndTags,
				releases, up,
			)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}

			got := make(map[string]string)
			for _, d := range decisions {
				parts := []string{string(d.Status)}
				if d.Lndinit != "" {
					parts = append(parts, d.Lndinit)
				}
				parts = append(parts, d.Detail)
				got[d.Lnd] = strings.Join(parts, " ")
			}

			if len(got) != len(test.want) {
				t.Fatalf("got %d decisions %v, want %d",
					len(got), got, len(test.want))
			}
			for tag, want := range test.want {
				if got[tag] != want {
					t.Errorf("%s: got %q, want %q", tag,
						got[tag], want)
				}
			}
		})
	}
}

// TestPlanSearchMiss checks that an image the search missed but an exact
// lookup finds stops the pass instead of being built again.
func TestPlanSearchMiss(t *testing.T) {
	up := readyUpstream("v0.20.6-beta")
	image := imageRepo + ":v0.1.34-beta-lnd-v0.20.6-beta"
	up.images[image] = true
	up.hidden[image] = true

	_, err := plan(
		context.Background(), []string{"v0.20"},
		[]string{"v0.20.6-beta"}, testReleases, up,
	)
	if err == nil || !strings.Contains(err.Error(), "did not return it") {
		t.Fatalf("got error %v, want a search disagreement", err)
	}
}
