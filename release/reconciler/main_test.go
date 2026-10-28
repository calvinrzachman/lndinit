package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestParseLines checks that -lines only accepts major lines.
func TestParseLines(t *testing.T) {
	t.Parallel()

	lines, err := parseLines(" v0.20, v0.21 ")
	if err != nil || !reflect.DeepEqual(lines, []string{"v0.20", "v0.21"}) {
		t.Fatalf("got (%v, %v)", lines, err)
	}

	// Each of these used to give an empty report with exit status 0.
	for _, bad := range []string{
		"", "0.21", "V0.21", "v0.21.4", "v0.21,", "v0.021",
	} {
		if _, err := parseLines(bad); err == nil {
			t.Fatalf("%q: expected an error", bad)
		}
	}
}

// TestStartRejectsArguments checks that leftover arguments fail before any
// network access. "-include-superseded-rcs false" leaves "false" behind and
// used to turn the option on.
func TestStartRejectsArguments(t *testing.T) {
	t.Parallel()

	cfg := config{lines: "v0.20", format: "text", hubURL: defaultHubURL}
	err := start(cfg, []string{"false"}, &bytes.Buffer{})
	if err == nil ||
		!strings.Contains(err.Error(), "unexpected arguments") {

		t.Fatalf("expected an arguments error, got %v", err)
	}
}

// testSources returns in-memory sources for one build and one present
// release.
func testSources() *sources {
	reg := newFakeRegistry()
	reg.add(testBaseRepo, "v0.20.3-beta", "v0.20.4-beta")
	reg.add(testImageRepo, "v0.1.34-beta-lnd-v0.20.3-beta")

	return &sources{
		lndTags: func(context.Context) ([]string, error) {
			return []string{"v0.20.3-beta", "v0.20.4-beta"}, nil
		},
		releases: func(context.Context) ([]lndinitRelease, error) {
			return knownReleases, nil
		},
		dockerTags: func(context.Context) ([]string, error) {
			return nil, nil
		},
		registry: reg,
		upstream: newFakeUpstream(),
	}
}

// TestRunReports checks the report contract end to end: the JSON file, the
// appended step summary and the text on stdout all come from one pass.
func TestRunReports(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := config{
		format:     "text",
		baseRepo:   testBaseRepo,
		imageRepo:  testImageRepo,
		jsonOut:    filepath.Join(dir, "decisions.json"),
		summaryOut: filepath.Join(dir, "summary.md"),
	}

	// A step summary may already hold other steps' output.
	err := os.WriteFile(cfg.summaryOut, []byte("earlier step\n"), 0644)
	if err != nil {
		t.Fatalf("write summary: %v", err)
	}

	var stdout bytes.Buffer
	err = run(context.Background(), cfg, []string{"v0.20"}, testSources(),
		&stdout)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	raw, err := os.ReadFile(cfg.jsonOut)
	if err != nil {
		t.Fatalf("read json: %v", err)
	}
	var rep report
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatalf("decode json: %v", err)
	}
	if rep.SchemaVersion != reportSchemaVersion || !rep.DryRun ||
		!reflect.DeepEqual(rep.Inputs.Lines, []string{"v0.20"}) {

		t.Fatalf("unexpected envelope: %+v", rep)
	}
	want := []string{"v0.20.3-beta=present", "v0.20.4-beta=build"}
	if got := statuses(rep.Decisions); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	summary, err := os.ReadFile(cfg.summaryOut)
	if err != nil {
		t.Fatalf("read summary: %v", err)
	}
	if !strings.HasPrefix(string(summary), "earlier step\n") ||
		!strings.Contains(string(summary),
			"| v0.20.4-beta | v0.20 | build |") {

		t.Fatalf("unexpected summary:\n%s", summary)
	}

	if !strings.Contains(stdout.String(), "1 build") {
		t.Fatalf("unexpected stdout:\n%s", stdout.String())
	}

	// The JSON file is replaced through a rename, so no temporary file
	// is left next to it.
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("expected only the two reports, got %v (%v)",
			entries, err)
	}
}

// TestRunSourceError checks that a failed source aborts the pass and leaves
// an earlier JSON report untouched.
func TestRunSourceError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := config{
		format:  "text",
		jsonOut: filepath.Join(dir, "decisions.json"),
	}
	if err := os.WriteFile(cfg.jsonOut, []byte("old"), 0644); err != nil {
		t.Fatalf("write json: %v", err)
	}

	src := testSources()
	src.releases = func(context.Context) ([]lndinitRelease, error) {
		return nil, errors.New("no tags")
	}

	err := run(context.Background(), cfg, []string{"v0.20"}, src,
		&bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "no tags") {
		t.Fatalf("expected source error, got %v", err)
	}

	raw, _ := os.ReadFile(cfg.jsonOut)
	if string(raw) != "old" {
		t.Fatalf("earlier report was changed: %q", raw)
	}
}

// failWriter fails every write.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) {
	return 0, errors.New("disk full")
}

// TestWritersReportErrors checks that write errors are returned rather than
// dropped.
func TestWritersReportErrors(t *testing.T) {
	t.Parallel()

	rep := &report{Decisions: []decision{{Lnd: "v0.20.4-beta"}}}
	for name, write := range writers {
		if err := write(failWriter{}, rep); err == nil {
			t.Fatalf("%s: expected a write error", name)
		}
	}
}
