package main

import (
	"reflect"
	"strings"
	"testing"
)

// TestParseLines checks that only major lines are accepted.
func TestParseLines(t *testing.T) {
	got, err := parseLines("v0.20, v0.21")
	if err != nil || !reflect.DeepEqual(got, []string{"v0.20", "v0.21"}) {
		t.Fatalf("got %v, %v", got, err)
	}

	for _, s := range []string{"", "0.21", "v0.21.4", "v0.21,", "v0.021"} {
		if _, err := parseLines(s); err == nil {
			t.Errorf("%q: expected an error", s)
		}
	}
}

// TestWriteTable checks the table and the status counts.
func TestWriteTable(t *testing.T) {
	var b strings.Builder
	err := writeTable(&b, []decision{{
		Lnd: "v0.20.6-beta", Line: "v0.20", Status: statusBuild,
		Lndinit: "v0.1.34-beta",
		Detail:  "v0.1.34-beta-lnd-v0.20.6-beta",
	}, {
		Lnd: "v0.20.5-beta", Line: "v0.20", Status: statusPresent,
		Detail: "v0.1.34-beta-lnd-v0.20.5-beta",
	}})
	if err != nil {
		t.Fatal(err)
	}

	out := b.String()
	for _, want := range []string{
		"v0.20.6-beta  v0.20  build    v0.1.34-beta  " +
			"v0.1.34-beta-lnd-v0.20.6-beta",
		"1 build, 0 wait-signatures, 0 wait-base-image, " +
			"0 needs-manual, 0 no-lndinit, 1 present, " +
			"0 superseded-rc",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}
