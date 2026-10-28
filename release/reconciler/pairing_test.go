package main

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"golang.org/x/mod/semver"
)

// knownReleases is the lndinit tag to lnd dependency mapping as of
// v0.1.38-beta, read from go.mod at each tag.
var knownReleases = []lndinitRelease{
	{Tag: "v0.1.31-beta", LndDep: "v0.19.3-beta"},
	{Tag: "v0.1.32-beta", LndDep: "v0.20.0-beta.rc1"},
	{Tag: "v0.1.33-beta", LndDep: "v0.20.0-beta"},
	{Tag: "v0.1.34-beta", LndDep: "v0.20.1-beta"},
	{Tag: "v0.1.35-beta", LndDep: "v0.21.0-beta.rc1"},
	{Tag: "v0.1.36-beta", LndDep: "v0.21.0-beta"},
	{
		Tag:    "v0.1.37-beta",
		LndDep: "v0.21.0-beta.rc2.0.20260717013942-f02cf4c470c9",
	},
	{
		Tag:    "v0.1.38-beta",
		LndDep: "v0.21.0-beta.rc2.0.20260717013942-f02cf4c470c9",
	},
}

// TestPickLndinit checks the pairing rule against the known release history.
func TestPickLndinit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		releases []lndinitRelease
		line     string
		want     string
		ok       bool
	}{{
		// A maintenance release on an older line gets the newest
		// lndinit for that line, not the newest lndinit overall. This
		// is the v0.20.5-beta.rc1 case.
		name:     "older line",
		releases: knownReleases,
		line:     "v0.20",
		want:     "v0.1.34-beta",
		ok:       true,
	}, {
		// Pseudo-versions count towards their line.
		name:     "current line via pseudo-version",
		releases: knownReleases,
		line:     "v0.21",
		want:     "v0.1.38-beta",
		ok:       true,
	}, {
		// A new major line has no lndinit yet and must fail closed.
		name:     "new line",
		releases: knownReleases,
		line:     "v0.22",
		ok:       false,
	}, {
		// Version tags compare numerically, not lexically.
		name: "numeric compare",
		releases: []lndinitRelease{
			{Tag: "v0.1.9-beta", LndDep: "v0.20.0-beta"},
			{Tag: "v0.1.10-beta", LndDep: "v0.20.1-beta"},
		},
		line: "v0.20",
		want: "v0.1.10-beta",
		ok:   true,
	}, {
		// Non-release tags never pair, even if they carry a go.mod.
		name: "ignore non-release tags",
		releases: []lndinitRelease{
			{Tag: "v0.1.34-beta", LndDep: "v0.20.1-beta"},
			{
				Tag:    "docker/v0.1.99-beta-lnd-v0.20.5-beta",
				LndDep: "v0.20.5-beta",
			},
			{Tag: "v0.1.99-beta.rc1", LndDep: "v0.20.5-beta"},
		},
		line: "v0.20",
		want: "v0.1.34-beta",
		ok:   true,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := pickLndinit(tc.releases, tc.line)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("got (%q, %v), want (%q, %v)", got, ok,
					tc.want, tc.ok)
			}
		})
	}
}

// TestDepFlags checks which lnd dependencies are flagged for a closer look.
func TestDepFlags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		dep  string
		want []string
	}{
		{dep: "v0.20.1-beta"},
		{dep: "v0.21.0-beta.rc1", want: []string{"rc-dep"}},
		{
			dep: "v0.21.0-beta.rc2.0.20260717013942-" +
				"f02cf4c470c9",
			want: []string{"pseudo-version-dep"},
		},
	}

	for _, tc := range tests {
		if got := depFlags(tc.dep); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s: got %v, want %v", tc.dep, got, tc.want)
		}
	}
}

// TestChooseLndinit covers the cases the planner tests don't: a line with
// only pseudo-version releases, and a failing ancestry lookup.
func TestChooseLndinit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	linear := func(_ context.Context, older, newer string) (bool, error) {
		return semver.Compare(older, newer) <= 0, nil
	}

	onlyPseudo := []lndinitRelease{{
		Tag:    "v0.1.40-beta",
		LndDep: "v0.22.0-beta.rc1.0.20270101000000-abcdefabcdef",
	}}
	rel, manual, found, err := chooseLndinit(
		ctx, onlyPseudo, "v0.22", linear,
	)
	if err != nil || !found || !manual || rel.Tag != "v0.1.40-beta" {
		t.Fatalf("only pseudo: got (%+v, manual=%v, found=%v, %v)",
			rel, manual, found, err)
	}

	failing := func(context.Context, string, string) (bool, error) {
		return false, errors.New("unknown revision")
	}
	_, _, _, err = chooseLndinit(ctx, knownReleases, "v0.21", failing)
	if err == nil {
		t.Fatalf("expected the ancestry error")
	}

	// v0.20 has no pseudo-version releases, so ancestry isn't consulted.
	rel, manual, found, err = chooseLndinit(
		ctx, knownReleases, "v0.20", failing,
	)
	if err != nil || !found || manual || rel.Tag != "v0.1.34-beta" {
		t.Fatalf("v0.20: got (%+v, manual=%v, found=%v, %v)", rel,
			manual, found, err)
	}
}
