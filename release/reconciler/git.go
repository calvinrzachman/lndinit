package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"golang.org/x/mod/modfile"
)

// lndModule is the module path whose version pins the lnd code compiled into
// the lndinit binary.
const lndModule = "github.com/lightningnetwork/lnd"

// runGit runs git with the given arguments and returns its stdout.
func runGit(ctx context.Context, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s",
			strings.Join(args, " "), err,
			strings.TrimSpace(stderr.String()))
	}

	return stdout.Bytes(), nil
}

// remoteTags lists the v* tags of a remote repository without cloning it.
func remoteTags(ctx context.Context, remote string) ([]string, error) {
	// A remote starting with a dash would be read as an option, and
	// options like --upload-pack run commands.
	if remote == "" || strings.HasPrefix(remote, "-") {
		return nil, fmt.Errorf("invalid remote %q", remote)
	}

	out, err := runGit(
		ctx, "ls-remote", "--tags", "--refs", "--", remote,
		"refs/tags/v*",
	)
	if err != nil {
		return nil, err
	}

	var tags []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}

		tag, ok := strings.CutPrefix(fields[1], "refs/tags/")
		if ok {
			tags = append(tags, tag)
		}
	}

	return tags, nil
}

// lndinitReleases reads every lndinit version tag in the repository at dir
// together with the lnd version its go.mod requires. Tags whose go.mod does
// not require lnd are skipped. A tag without a go.mod, or with one that
// can't be read, fails the whole pass rather than being skipped.
func lndinitReleases(ctx context.Context,
	dir string) ([]lndinitRelease, error) {

	out, err := runGit(ctx, "-C", dir, "tag", "--list", "v*")
	if err != nil {
		return nil, err
	}

	var releases []lndinitRelease
	for _, tag := range strings.Fields(string(out)) {
		if !lndinitTagRe.MatchString(tag) {
			continue
		}

		goMod, err := runGit(ctx, "-C", dir, "show", tag+":go.mod")
		if err != nil {
			return nil, err
		}

		dep, err := requiredVersion(goMod, lndModule)
		if err != nil {
			return nil, fmt.Errorf("go.mod at %s: %w", tag, err)
		}
		if dep == "" {
			continue
		}

		releases = append(releases, lndinitRelease{
			Tag:    tag,
			LndDep: dep,
		})
	}

	return releases, nil
}

// requiredVersion returns the version of module that a go.mod file requires,
// or an empty string if it does not require it.
//
// A replace directive for the module is an error. The version in the require
// line would then no longer describe the code that is compiled in, and the
// pairing rule would be reasoning about the wrong lnd.
func requiredVersion(goMod []byte, module string) (string, error) {
	// Parse rather than ParseLax: the lax parser skips replace
	// directives, which are exactly what we need to see.
	f, err := modfile.Parse("go.mod", goMod, nil)
	if err != nil {
		return "", err
	}

	for _, r := range f.Replace {
		if r.Old.Path == module {
			return "", fmt.Errorf("%s is replaced by %s %s", module,
				r.New.Path, r.New.Version)
		}
	}

	for _, r := range f.Require {
		if r.Mod.Path == module {
			return r.Mod.Version, nil
		}
	}

	return "", nil
}

// dockerTags lists the docker/ build tags in the repository at dir.
func dockerTags(ctx context.Context, dir string) ([]string, error) {
	out, err := runGit(ctx, "-C", dir, "tag", "--list", "docker/*")
	if err != nil {
		return nil, err
	}

	return strings.Fields(string(out)), nil
}

// isAncestor reports whether commit-ish older is an ancestor of newer in the
// repository at dir.
func isAncestor(ctx context.Context, dir, older, newer string) (bool, error) {
	cmd := exec.CommandContext(
		ctx, "git", "-C", dir, "merge-base", "--is-ancestor", "--",
		older, newer,
	)
	err := cmd.Run()

	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return true, nil

	// Exit status 1 means "not an ancestor". Anything else, such as an
	// unknown tag, is an error.
	case errors.As(err, &exitErr) && exitErr.ExitCode() == 1:
		return false, nil
	}

	return false, fmt.Errorf("git merge-base --is-ancestor %s %s: %w",
		older, newer, err)
}
