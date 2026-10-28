// Command reconciler decides which lndinit container images are missing for
// lnd releases and which lndinit version each one should contain.
//
// It only reads: lnd's tags over git, lndinit's tags and go.mod history from
// a local checkout, and Docker Hub anonymously. It never builds, pushes or
// tags anything.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"
)

// config holds the command line flags.
type config struct {
	lines                string
	repo                 string
	lndRemote            string
	baseRepo             string
	imageRepo            string
	hubURL               string
	githubURL            string
	lndRepo              string
	includeSupersededRCs bool
	format               string
	jsonOut              string
	summaryOut           string
	timeout              time.Duration
}

func main() {
	var cfg config
	flag.StringVar(&cfg.lines, "lines", "v0.20,v0.21",
		"comma separated lnd major lines to reconcile")
	flag.StringVar(&cfg.repo, "repo", ".",
		"path to an lndinit checkout with all version tags fetched")
	flag.StringVar(&cfg.lndRemote, "lnd-remote",
		"https://github.com/lightningnetwork/lnd.git",
		"git remote to read lnd release tags from")
	flag.StringVar(&cfg.baseRepo, "base-repo", "lightninglabs/lnd",
		"Docker Hub repository of the lnd base image")
	flag.StringVar(&cfg.imageRepo, "image-repo", "lightninglabs/lndinit",
		"Docker Hub repository of the lndinit image")
	flag.StringVar(&cfg.hubURL, "hub-url", defaultHubURL,
		"Docker Hub API endpoint")
	flag.StringVar(&cfg.githubURL, "github-url", defaultGitHubURL,
		"GitHub API endpoint")
	flag.StringVar(&cfg.lndRepo, "lnd-repo", defaultLndRepo,
		"GitHub repository lnd releases are published in")
	flag.BoolVar(&cfg.includeSupersededRCs, "include-superseded-rcs",
		false, "also build rcs superseded by a later rc or the final")
	flag.StringVar(&cfg.format, "format", "text",
		"output format: text, json or markdown")
	flag.StringVar(&cfg.jsonOut, "json-out", "",
		"also write the decisions as JSON to this file")
	flag.StringVar(&cfg.summaryOut, "summary-out", "",
		"also append the decisions as Markdown to this file, such as "+
			"$GITHUB_STEP_SUMMARY")
	flag.DurationVar(&cfg.timeout, "timeout", 5*time.Minute,
		"overall time limit for the pass")
	flag.Parse()

	if err := start(cfg, flag.Args(), os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "reconciler: %v\n", err)
		os.Exit(1)
	}
}

// lineArgRe matches an lnd major line as given to -lines, for example v0.21.
var lineArgRe = regexp.MustCompile(`^v(0|[1-9]\d{0,3})\.(0|[1-9]\d{0,3})$`)

// parseLines parses the -lines flag. Every entry must name a major line, so
// that a typo such as 0.21 or v0.21.4 fails instead of silently matching no
// release and producing an empty report.
func parseLines(s string) ([]string, error) {
	var lines []string
	for _, line := range strings.Split(s, ",") {
		line = strings.TrimSpace(line)
		if !lineArgRe.MatchString(line) {
			return nil, fmt.Errorf("invalid lnd line %q in "+
				"-lines, want a major line such as v0.21", line)
		}

		lines = append(lines, line)
	}

	return lines, nil
}

// start validates the command line, then runs one pass against the live
// sources.
func start(cfg config, args []string, w io.Writer) error {
	// Leftover arguments mean something was not parsed as intended. The
	// usual cause is a boolean written as "-flag false", which sets the
	// flag and leaves "false" behind.
	if len(args) > 0 {
		return fmt.Errorf("unexpected arguments %q: boolean flags "+
			"take -flag=value, and all flags must come first", args)
	}

	lines, err := parseLines(cfg.lines)
	if err != nil {
		return err
	}
	if _, ok := writers[cfg.format]; !ok {
		return fmt.Errorf("unknown format %q", cfg.format)
	}

	hub, err := newDockerHub(
		&http.Client{Timeout: 30 * time.Second}, cfg.hubURL,
	)
	if err != nil {
		return err
	}

	// The token only raises GitHub's rate limit. It is read from the
	// environment rather than a flag so it never shows up in a process
	// listing.
	gh, err := newGitHub(
		&http.Client{Timeout: 30 * time.Second}, cfg.githubURL,
		cfg.lndRepo, os.Getenv("GITHUB_TOKEN"),
	)
	if err != nil {
		return err
	}

	src := &sources{
		lndTags: func(ctx context.Context) ([]string, error) {
			return remoteTags(ctx, cfg.lndRemote)
		},
		releases: func(ctx context.Context) ([]lndinitRelease, error) {
			return lndinitReleases(ctx, cfg.repo)
		},
		dockerTags: func(ctx context.Context) ([]string, error) {
			return dockerTags(ctx, cfg.repo)
		},
		registry: hub,
		upstream: &liveUpstream{
			gitHub:    gh,
			lndSource: newLndSource(),
			repo:      cfg.repo,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()

	return run(ctx, cfg, lines, src, w)
}

// liveUpstream answers upstream questions from GitHub and the local lndinit
// checkout.
type liveUpstream struct {
	*gitHub
	*lndSource

	// repo is the path of the lndinit checkout.
	repo string
}

// IsAncestor reports whether lndinit tag older is an ancestor of newer in the
// local checkout.
func (u *liveUpstream) IsAncestor(ctx context.Context, older,
	newer string) (bool, error) {

	return isAncestor(ctx, u.repo, older, newer)
}

// sources is where a pass reads its inputs from. Tests replace it with
// in-memory data.
type sources struct {
	lndTags    func(context.Context) ([]string, error)
	releases   func(context.Context) ([]lndinitRelease, error)
	dockerTags func(context.Context) ([]string, error)
	registry   registry
	upstream   upstream
}

// run performs one reconcile pass over the given lines and writes the report
// to w, and to the report files if configured.
func run(ctx context.Context, cfg config, lines []string, src *sources,
	w io.Writer) error {

	lndTags, err := src.lndTags(ctx)
	if err != nil {
		return fmt.Errorf("listing lnd tags: %w", err)
	}

	releases, err := src.releases(ctx)
	if err != nil {
		return fmt.Errorf("reading lndinit releases: %w", err)
	}

	pushed, err := src.dockerTags(ctx)
	if err != nil {
		return fmt.Errorf("reading docker tags: %w", err)
	}

	decisions, err := plan(ctx, planConfig{
		Lines:                lines,
		BaseRepo:             cfg.baseRepo,
		ImageRepo:            cfg.imageRepo,
		IncludeSupersededRCs: cfg.includeSupersededRCs,
	}, planInputs{
		LndTags:    lndTags,
		Releases:   releases,
		DockerTags: pushed,
	}, src.registry, src.upstream)
	if err != nil {
		return err
	}

	rep := &report{
		SchemaVersion: reportSchemaVersion,
		DryRun:        true,
		Inputs: reportInputs{
			Lines:                lines,
			LndRemote:            cfg.lndRemote,
			BaseRepo:             cfg.baseRepo,
			ImageRepo:            cfg.imageRepo,
			IncludeSupersededRCs: cfg.includeSupersededRCs,
		},
		Decisions: decisions,
	}

	if cfg.jsonOut != "" {
		err := replaceFile(cfg.jsonOut, writeJSON, rep)
		if err != nil {
			return err
		}
	}

	// A step summary is shared by every step of a job, so it is appended
	// to rather than replaced.
	if cfg.summaryOut != "" {
		err := appendFile(cfg.summaryOut, writeMarkdown, rep)
		if err != nil {
			return err
		}
	}

	return writers[cfg.format](w, rep)
}

// reportSchemaVersion is the version of the JSON report layout. It changes
// whenever a field is renamed, removed or changes meaning, so that anything
// consuming the report can refuse a layout it doesn't understand.
const reportSchemaVersion = 1

// report is the result of one reconcile pass.
type report struct {
	// SchemaVersion is reportSchemaVersion.
	SchemaVersion int `json:"schema_version"`

	// DryRun is always true for now: the reconciler only plans.
	DryRun bool `json:"dry_run"`

	// Inputs records the settings the pass ran with.
	Inputs reportInputs `json:"inputs"`

	// Decisions holds one entry per lnd release on the configured lines.
	Decisions []decision `json:"decisions"`
}

// reportInputs records the settings a pass ran with, so a report can be
// read without the command line that produced it.
type reportInputs struct {
	Lines                []string `json:"lines"`
	LndRemote            string   `json:"lnd_remote"`
	BaseRepo             string   `json:"base_repo"`
	ImageRepo            string   `json:"image_repo"`
	IncludeSupersededRCs bool     `json:"include_superseded_rcs"`
}

// writers maps each output format to the function that renders it.
var writers = map[string]func(io.Writer, *report) error{
	"text":     writeText,
	"json":     writeJSON,
	"markdown": writeMarkdown,
}

// replaceFile renders the report into a temporary file next to path and
// renames it into place, so path holds either the previous report or the
// complete new one, never a partial write.
func replaceFile(path string, render func(io.Writer, *report) error,
	rep *report) error {

	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path))
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	// CreateTemp makes the file private. The report holds only public
	// data, and readers such as an artifact upload expect the usual mode.
	if err := tmp.Chmod(0644); err != nil {
		_ = tmp.Close()
		return err
	}

	if err := render(tmp, rep); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}

	return os.Rename(tmp.Name(), path)
}

// appendFile renders the report onto the end of the file at path.
func appendFile(path string, render func(io.Writer, *report) error,
	rep *report) error {

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}

	if err := render(f, rep); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}

	return f.Close()
}

// errWriter wraps a writer and remembers the first write error, so a
// sequence of prints can be checked once at the end.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}

	var n int
	n, e.err = e.w.Write(p)
	return n, e.err
}

// writeJSON writes the report as indented JSON.
func writeJSON(w io.Writer, rep *report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	return enc.Encode(rep)
}

// detail returns the last column of the report: the image to build, the
// images that already exist, or a short explanation.
func detail(d decision) string {
	switch d.Status {
	case statusPresent:
		return strings.Join(d.Existing, ", ")

	case statusPresentMismatch:
		return "no image built for " + d.Line + ": " +
			strings.Join(d.Existing, ", ")

	case statusBuild:
		return d.Image

	case statusWaitBase:
		return "waiting for the lnd image"

	case statusWaitSignatures:
		return "lnd release not published yet"

	case statusTaggedNoImage:
		return "build running or failed: " +
			strings.Join(d.Pending, ", ")

	case statusDBMismatch:
		return "database versions differ: " + d.Note

	case statusNeedsManual:
		return "newest lndinit for " + d.Line + " is on an lnd " +
			"pseudo-version; build by hand"

	case statusNoLndinit:
		return "needs a lndinit release for " + d.Line

	case statusSuperseded:
		return "a later rc or the final release is tagged"
	}

	return ""
}

// counts tallies decisions by status, in a fixed order.
func counts(decisions []decision) string {
	order := []status{
		statusBuild, statusWaitSignatures, statusWaitBase,
		statusTaggedNoImage, statusDBMismatch, statusNeedsManual,
		statusNoLndinit,
		statusPresentMismatch, statusPresent, statusSuperseded,
	}

	n := make(map[status]int)
	for _, d := range decisions {
		n[d.Status]++
	}

	parts := make([]string, 0, len(order))
	for _, s := range order {
		parts = append(parts, fmt.Sprintf("%d %s", n[s], s))
	}

	return strings.Join(parts, ", ")
}

// ruleColumn returns the rule's lndinit choice with any flags, for the text
// and Markdown tables.
func ruleColumn(d decision) string {
	if len(d.Flags) == 0 {
		return d.RuleLndinit
	}

	return d.RuleLndinit + " (" + strings.Join(d.Flags, ", ") + ")"
}

// writeText writes the report as an aligned table.
func writeText(w io.Writer, rep *report) error {
	ew := &errWriter{w: w}
	tw := tabwriter.NewWriter(ew, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "LND\tLINE\tSTATUS\tRULE LNDINIT\tDETAIL")
	for _, d := range rep.Decisions {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", d.Lnd, d.Line,
			d.Status, ruleColumn(d), detail(d))
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintf(ew, "\n%s\n", counts(rep.Decisions))

	return ew.err
}

// writeMarkdown writes the report as a Markdown table, suitable for a
// GitHub Actions step summary.
func writeMarkdown(w io.Writer, rep *report) error {
	ew := &errWriter{w: w}
	fmt.Fprintln(ew, "## lndinit image reconcile (dry run)")
	fmt.Fprintln(ew)
	fmt.Fprintln(ew, counts(rep.Decisions))
	fmt.Fprintln(ew)
	fmt.Fprintln(ew, "| lnd | line | status | rule lndinit | detail |")
	fmt.Fprintln(ew, "|---|---|---|---|---|")
	for _, d := range rep.Decisions {
		fmt.Fprintf(ew, "| %s | %s | %s | %s | %s |\n", d.Lnd,
			d.Line, d.Status, ruleColumn(d), detail(d))
	}

	return ew.err
}
