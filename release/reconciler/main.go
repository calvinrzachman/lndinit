// Command reconciler reports which lndinit container images are missing for
// lnd releases and which lndinit version each one should contain.
//
// It only reads: lnd's tags over git, lndinit's tags and go.mod history from
// a local checkout, lnd's releases from GitHub and image tags from Docker
// Hub. It never builds, pushes or tags anything.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"
)

const (
	// lndRemote is the git remote lnd release tags are read from.
	lndRemote = "https://github.com/lightningnetwork/lnd.git"

	// lndRepo is the GitHub repository lnd releases are published in.
	lndRepo = "lightningnetwork/lnd"
)

func main() {
	lines := flag.String("lines", "v0.20,v0.21",
		"comma separated lnd major lines to reconcile")
	repo := flag.String("repo", ".",
		"path to an lndinit checkout with full history and all tags")
	timeout := flag.Duration("timeout", 5*time.Minute,
		"time limit for the pass")
	flag.Parse()

	if err := run(*lines, *repo, *timeout, flag.Args()); err != nil {
		fmt.Fprintf(os.Stderr, "reconciler: %v\n", err)
		os.Exit(1)
	}
}

// run performs one reconcile pass against the live sources and prints the
// result.
func run(linesFlag, repo string, timeout time.Duration,
	args []string) error {

	if len(args) > 0 {
		return fmt.Errorf("unexpected arguments %q", args)
	}

	lines, err := parseLines(linesFlag)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	lndTags, err := remoteTags(ctx, lndRemote)
	if err != nil {
		return fmt.Errorf("listing lnd tags: %w", err)
	}

	releases, err := lndinitReleases(ctx, repo)
	if err != nil {
		return fmt.Errorf("reading lndinit releases: %w", err)
	}

	client := newHTTPClient(&http.Client{})
	up := &liveUpstream{
		dockerHub: &dockerHub{client: client, baseURL: hubURL},
		gitHub: &gitHub{
			client:  client,
			baseURL: gitHubURL,
			repo:    lndRepo,
			token:   os.Getenv("GITHUB_TOKEN"),
		},
		repo: repo,
	}

	decisions, err := plan(ctx, lines, lndTags, releases, up)
	if err != nil {
		return err
	}

	return writeTable(os.Stdout, decisions)
}

// liveUpstream answers upstream questions from Docker Hub, GitHub and the
// local lndinit checkout.
type liveUpstream struct {
	*dockerHub
	*gitHub

	// repo is the path of the lndinit checkout.
	repo string
}

// IsAncestor reports whether lndinit tag older is an ancestor of newer in the
// local checkout.
func (u *liveUpstream) IsAncestor(ctx context.Context, older,
	newer string) (bool, error) {

	return isAncestor(ctx, u.repo, older, newer)
}

// lineArgRe matches an lnd major line as given to -lines, for example v0.21.
var lineArgRe = regexp.MustCompile(`^v(0|[1-9]\d{0,3})\.(0|[1-9]\d{0,3})$`)

// parseLines parses the -lines flag. A typo such as 0.21 or v0.21.4 fails
// instead of silently matching no release.
func parseLines(s string) ([]string, error) {
	var lines []string
	for _, line := range strings.Split(s, ",") {
		line = strings.TrimSpace(line)
		if !lineArgRe.MatchString(line) {
			return nil, fmt.Errorf("invalid lnd line %q, want a "+
				"major line such as v0.21", line)
		}

		lines = append(lines, line)
	}

	return lines, nil
}

// writeTable writes the decisions as an aligned table followed by a count of
// each status.
func writeTable(w io.Writer, decisions []decision) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "LND\tLINE\tSTATUS\tLNDINIT\tDETAIL")

	n := make(map[status]int)
	for _, d := range decisions {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", d.Lnd, d.Line,
			d.Status, d.Lndinit, d.Detail)
		n[d.Status]++
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	parts := make([]string, 0, len(statusOrder))
	for _, s := range statusOrder {
		parts = append(parts, fmt.Sprintf("%d %s", n[s], s))
	}

	_, err := fmt.Fprintf(w, "\n%s\n", strings.Join(parts, ", "))
	return err
}
