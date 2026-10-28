package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// git runs git in dir and fails the test on error.
func git(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// commitGoMod commits a go.mod that requires lnd at dep, or nothing if dep
// is empty, and tags the commit.
func commitGoMod(t *testing.T, dir, dep, tag string) {
	t.Helper()

	goMod := "module github.com/lightninglabs/lndinit\n\ngo 1.25\n"
	if dep != "" {
		goMod += "\nrequire " + lndModule + " " + dep + "\n"
	}
	err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0644)
	if err != nil {
		t.Fatal(err)
	}

	git(t, dir, "add", "go.mod")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", tag)
	git(t, dir, "tag", tag)
}

// newRepo returns a git repository with this history:
//
//	v0.1.1-beta -- v0.1.2-beta -- v0.1.4-beta   (main)
//	      \
//	       v0.1.3-beta                          (maintenance branch)
//
// v0.1.2-beta does not require lnd. The repository also has a docker/ tag
// and a non-release tag, which must both be ignored.
func newRepo(t *testing.T) string {
	t.Helper()

	// Keep the user's git config, such as tag signing, out of the test.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")

	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")

	commitGoMod(t, dir, "v0.20.1-beta", "v0.1.1-beta")
	commitGoMod(t, dir, "", "v0.1.2-beta")
	commitGoMod(t, dir, "v0.21.0-beta", "v0.1.4-beta")
	git(t, dir, "tag", "docker/v0.1.4-beta-lnd-v0.21.0-beta")
	git(t, dir, "tag", "v0.1.5")

	git(t, dir, "checkout", "-q", "-b", "maint", "v0.1.1-beta")
	commitGoMod(t, dir, "v0.20.2-beta", "v0.1.3-beta")

	return dir
}

// TestLndinitReleases checks that releases are read with their lnd
// dependency and that everything else is skipped.
func TestLndinitReleases(t *testing.T) {
	dir := newRepo(t)

	got, err := lndinitReleases(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}

	want := []lndinitRelease{
		{Tag: "v0.1.1-beta", LndDep: "v0.20.1-beta"},
		{Tag: "v0.1.3-beta", LndDep: "v0.20.2-beta"},
		{Tag: "v0.1.4-beta", LndDep: "v0.21.0-beta"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestLndinitReleasesReplace checks that a go.mod replacing lnd fails the
// pass.
func TestLndinitReleasesReplace(t *testing.T) {
	dir := newRepo(t)

	goMod := "module x\n\nrequire " + lndModule + " v0.21.0-beta\n\n" +
		"replace " + lndModule + " => ../lnd\n"
	err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0644)
	if err != nil {
		t.Fatal(err)
	}
	git(t, dir, "commit", "-q", "-am", "replace")
	git(t, dir, "tag", "v0.1.9-beta")

	_, err = lndinitReleases(context.Background(), dir)
	if err == nil || !strings.Contains(err.Error(), "replaced") {
		t.Fatalf("got error %v, want a replace error", err)
	}
}

// TestLndinitReleasesShallow checks that a shallow clone is refused, since
// it would answer ancestry questions wrongly.
func TestLndinitReleasesShallow(t *testing.T) {
	dir := newRepo(t)

	clone := filepath.Join(t.TempDir(), "clone")
	cmd := exec.Command(
		"git", "clone", "-q", "--depth", "1", "--no-single-branch",
		"file://"+dir, clone,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone: %v: %s", err, out)
	}

	_, err := lndinitReleases(context.Background(), clone)
	if err == nil || !strings.Contains(err.Error(), "shallow") {
		t.Fatalf("got error %v, want a shallow clone error", err)
	}
}

// TestIsAncestor checks the three answers git can give.
func TestIsAncestor(t *testing.T) {
	dir := newRepo(t)
	ctx := context.Background()

	tests := []struct {
		older, newer string
		want         bool
	}{
		{"v0.1.1-beta", "v0.1.4-beta", true},
		{"v0.1.4-beta", "v0.1.1-beta", false},
		{"v0.1.3-beta", "v0.1.4-beta", false},
	}
	for _, test := range tests {
		got, err := isAncestor(ctx, dir, test.older, test.newer)
		if err != nil {
			t.Fatal(err)
		}
		if got != test.want {
			t.Errorf("isAncestor(%s, %s) = %v, want %v",
				test.older, test.newer, got, test.want)
		}
	}

	if _, err := isAncestor(ctx, dir, "v9.9.9", "v0.1.4-beta"); err == nil {
		t.Error("expected an error for an unknown tag")
	}
}

// TestRemoteTags checks that only v* tags are listed.
func TestRemoteTags(t *testing.T) {
	dir := newRepo(t)

	got, err := remoteTags(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"v0.1.1-beta", "v0.1.2-beta", "v0.1.3-beta", "v0.1.4-beta",
		"v0.1.5",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
