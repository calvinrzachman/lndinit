package main

import (
	"context"
	"errors"
	"testing"
)

// TestChooseLndinit checks which lndinit release each lnd line gets.
func TestChooseLndinit(t *testing.T) {
	pseudo := "v0.21.0-beta.rc2.0.20260717013942-f02cf4c470c9"

	tests := []struct {
		name      string
		releases  []lndinitRelease
		ancestors map[string]bool
		line      string

		wantTag    string
		wantManual bool
		wantFound  bool
	}{{
		name: "newest release on the line, not the newest overall",
		releases: []lndinitRelease{
			{Tag: "v0.1.33-beta", LndDep: "v0.20.0-beta"},
			{Tag: "v0.1.34-beta", LndDep: "v0.20.1-beta"},
			{Tag: "v0.1.36-beta", LndDep: "v0.21.0-beta"},
		},
		line:      "v0.20",
		wantTag:   "v0.1.34-beta",
		wantFound: true,
	}, {
		name: "rc dependency counts for its line",
		releases: []lndinitRelease{
			{Tag: "v0.1.35-beta", LndDep: "v0.21.0-beta.rc1"},
		},
		line:      "v0.21",
		wantTag:   "v0.1.35-beta",
		wantFound: true,
	}, {
		name: "only a pseudo-version release",
		releases: []lndinitRelease{
			{Tag: "v0.1.38-beta", LndDep: pseudo},
		},
		line:       "v0.21",
		wantTag:    "v0.1.38-beta",
		wantManual: true,
		wantFound:  true,
	}, {
		name: "newer pseudo-version release on the same branch",
		releases: []lndinitRelease{
			{Tag: "v0.1.36-beta", LndDep: "v0.21.0-beta"},
			{Tag: "v0.1.37-beta", LndDep: pseudo},
			{Tag: "v0.1.38-beta", LndDep: pseudo},
		},
		ancestors: map[string]bool{
			"v0.1.36-beta..v0.1.37-beta": true,
			"v0.1.36-beta..v0.1.38-beta": true,
		},
		line:       "v0.21",
		wantTag:    "v0.1.38-beta",
		wantManual: true,
		wantFound:  true,
	}, {
		name: "pseudo-version release on another branch",
		releases: []lndinitRelease{
			{Tag: "v0.1.36-beta", LndDep: "v0.21.0-beta"},
			{Tag: "v0.1.38-beta", LndDep: pseudo},
		},
		line:      "v0.21",
		wantTag:   "v0.1.36-beta",
		wantFound: true,
	}, {
		name: "older pseudo-version release",
		releases: []lndinitRelease{
			{Tag: "v0.1.38-beta", LndDep: pseudo},
			{Tag: "v0.1.39-beta", LndDep: "v0.21.0-beta"},
		},
		line:      "v0.21",
		wantTag:   "v0.1.39-beta",
		wantFound: true,
	}, {
		name: "no release on the line",
		releases: []lndinitRelease{
			{Tag: "v0.1.34-beta", LndDep: "v0.20.1-beta"},
		},
		line: "v0.22",
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			isAncestor := func(_ context.Context, older,
				newer string) (bool, error) {

				return test.ancestors[older+".."+newer], nil
			}

			rel, manual, found, err := chooseLndinit(
				context.Background(), test.releases, test.line,
				isAncestor,
			)
			if err != nil {
				t.Fatalf("chooseLndinit: %v", err)
			}
			if rel.Tag != test.wantTag || manual != test.wantManual ||
				found != test.wantFound {

				t.Fatalf("got %s manual=%v found=%v, want %s "+
					"manual=%v found=%v", rel.Tag, manual,
					found, test.wantTag, test.wantManual,
					test.wantFound)
			}
		})
	}
}

// TestChooseLndinitAncestorError checks that a failed history lookup stops
// the choice rather than falling back to the tagged release.
func TestChooseLndinitAncestorError(t *testing.T) {
	releases := []lndinitRelease{
		{Tag: "v0.1.36-beta", LndDep: "v0.21.0-beta"},
		{
			Tag:    "v0.1.38-beta",
			LndDep: "v0.21.0-beta.rc2.0.20260717013942-f02cf4c470c9",
		},
	}
	isAncestor := func(context.Context, string, string) (bool, error) {
		return false, errors.New("unknown revision")
	}

	_, _, _, err := chooseLndinit(
		context.Background(), releases, "v0.21", isAncestor,
	)
	if err == nil {
		t.Fatal("expected an error")
	}
}
