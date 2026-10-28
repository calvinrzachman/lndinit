package main

import (
	"sort"
	"testing"
)

// TestParseLndTag checks which tags are treated as lnd releases.
func TestParseLndTag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		tag  string
		ok   bool
		line string
		rc   int
	}{
		{tag: "v0.20.5-beta", ok: true, line: "v0.20"},
		{tag: "v0.20.5-beta.rc1", ok: true, line: "v0.20", rc: 1},
		{tag: "v0.21.10-beta.rc12", ok: true, line: "v0.21", rc: 12},

		// Submodule tags, pre-beta naming and odd suffixes are not
		// releases we build images for.
		{tag: "kvdb/v1.4.16", ok: false},
		{tag: "v0.20.5", ok: false},
		{tag: "v0.20.5-beta.rc0", ok: false},
		{tag: "v0.20.5-beta.rc1-dirty", ok: false},
		{tag: "v0.20.5-alpha", ok: false},

		// Non-canonical spellings of a real release are rejected so
		// that one release can't be planned under several names.
		{tag: "v0.20.05-beta", ok: false},
		{tag: "v00.20.5-beta", ok: false},
		{tag: "v0.20.5-beta.rc01", ok: false},
		{tag: "v0.20.99999-beta", ok: false},
	}

	for _, tc := range tests {
		v, ok := parseLndTag(tc.tag)
		if ok != tc.ok {
			t.Fatalf("%s: got ok=%v, want %v", tc.tag, ok, tc.ok)
		}
		if !ok {
			continue
		}
		if v.Line() != tc.line {
			t.Fatalf("%s: got line %s, want %s", tc.tag, v.Line(),
				tc.line)
		}
		if v.RC != tc.rc {
			t.Fatalf("%s: got rc %d, want %d", tc.tag, v.RC, tc.rc)
		}
	}
}

// TestLndVersionOrder checks that release candidates sort before their final
// release and that numeric components compare as numbers.
func TestLndVersionOrder(t *testing.T) {
	t.Parallel()

	want := []string{
		"v0.20.4-beta",
		"v0.20.5-beta.rc1",
		"v0.20.5-beta.rc2",
		"v0.20.5-beta.rc10",
		"v0.20.5-beta",
		"v0.20.10-beta",
		"v0.21.0-beta.rc1",
	}

	shuffled := []string{
		want[4], want[6], want[0], want[3], want[1], want[5], want[2],
	}
	versions := make([]lndVersion, 0, len(shuffled))
	for _, tag := range shuffled {
		v, ok := parseLndTag(tag)
		if !ok {
			t.Fatalf("unable to parse %s", tag)
		}
		versions = append(versions, v)
	}

	sort.Slice(versions, func(i, j int) bool {
		return versions[i].Less(versions[j])
	})

	for i, v := range versions {
		if v.Tag != want[i] {
			t.Fatalf("position %d: got %s, want %s", i, v.Tag,
				want[i])
		}
	}
}

// TestLineOf checks line extraction from go.mod versions, including the
// pseudo-version lndinit v0.1.37-beta and later depend on.
func TestLineOf(t *testing.T) {
	t.Parallel()

	tests := []struct {
		version string
		line    string
		ok      bool
	}{
		{version: "v0.20.1-beta", line: "v0.20", ok: true},
		{version: "v0.21.0-beta.rc1", line: "v0.21", ok: true},
		{
			version: "v0.21.0-beta.rc2.0.20260717013942-" +
				"f02cf4c470c9",
			line: "v0.21",
			ok:   true,
		},
		{version: "", ok: false},
		{version: "master", ok: false},
	}

	for _, tc := range tests {
		line, ok := lineOf(tc.version)
		if ok != tc.ok || line != tc.line {
			t.Fatalf("%q: got (%q, %v), want (%q, %v)", tc.version,
				line, ok, tc.line, tc.ok)
		}
	}
}
