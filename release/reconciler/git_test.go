package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// gitIn runs git in dir with a fixed identity, failing the test on error.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.Command("git", append([]string{
		"-c", "user.name=test", "-c", "user.email=test@example.com",
		"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false",
	}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// commitGoMod commits a go.mod with the given contents and tags the commit.
func commitGoMod(t *testing.T, dir, goMod, tag string) {
	t.Helper()

	err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0644)
	if err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	gitIn(t, dir, "add", "go.mod")
	gitIn(t, dir, "commit", "-q", "-m", tag)
	gitIn(t, dir, "tag", tag)
}

// TestGitSources checks both git sources against a scratch repository laid
// out like lndinit's: version tags with a go.mod, a docker build tag and a
// tag from before go.mod required lnd.
func TestGitSources(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")

	const header = "module github.com/lightninglabs/lndinit\n\ngo 1.25\n\n"

	commitGoMod(t, dir, header, "v0.1.0-beta")
	commitGoMod(t, dir, header+
		"require github.com/lightningnetwork/lnd v0.20.1-beta\n",
		"v0.1.34-beta")
	gitIn(t, dir, "tag", "docker/v0.1.34-beta-lnd-v0.20.5-beta.rc1")
	commitGoMod(t, dir, header+"require (\n"+
		"\tgithub.com/lightningnetwork/lnd v0.21.0-beta.rc2.0."+
		"20260717013942-f02cf4c470c9\n"+
		"\tgithub.com/lightningnetwork/lnd/kvdb v1.4.16\n)\n",
		"v0.1.38-beta")

	ctx := context.Background()

	releases, err := lndinitReleases(ctx, dir)
	if err != nil {
		t.Fatalf("lndinitReleases: %v", err)
	}
	want := []lndinitRelease{
		{Tag: "v0.1.34-beta", LndDep: "v0.20.1-beta"},
		{
			Tag: "v0.1.38-beta",
			LndDep: "v0.21.0-beta.rc2.0.20260717013942-" +
				"f02cf4c470c9",
		},
	}
	if !reflect.DeepEqual(releases, want) {
		t.Fatalf("got %+v, want %+v", releases, want)
	}

	pushed, err := dockerTags(ctx, dir)
	if err != nil {
		t.Fatalf("dockerTags: %v", err)
	}
	wantPushed := []string{"docker/v0.1.34-beta-lnd-v0.20.5-beta.rc1"}
	if !reflect.DeepEqual(pushed, wantPushed) {
		t.Fatalf("got %v, want %v", pushed, wantPushed)
	}

	for _, tc := range []struct {
		older, newer string
		want         bool
	}{
		{older: "v0.1.34-beta", newer: "v0.1.38-beta", want: true},
		{older: "v0.1.38-beta", newer: "v0.1.34-beta", want: false},
	} {
		got, err := isAncestor(ctx, dir, tc.older, tc.newer)
		if err != nil || got != tc.want {
			t.Fatalf("isAncestor(%s, %s): got (%v, %v), want %v",
				tc.older, tc.newer, got, err, tc.want)
		}
	}
	_, err = isAncestor(ctx, dir, "v9.9.9", "v0.1.38-beta")
	if err == nil {
		t.Fatalf("expected an error for an unknown tag")
	}

	// ls-remote accepts a local path, which stands in for lnd's remote.
	tags, err := remoteTags(ctx, dir)
	if err != nil {
		t.Fatalf("remoteTags: %v", err)
	}
	sort.Strings(tags)
	wantTags := []string{"v0.1.0-beta", "v0.1.34-beta", "v0.1.38-beta"}
	if !reflect.DeepEqual(tags, wantTags) {
		t.Fatalf("got %v, want %v", tags, wantTags)
	}
}

// TestRequiredVersionReplace checks that a replaced lnd dependency fails
// instead of being paired by its require line.
func TestRequiredVersionReplace(t *testing.T) {
	t.Parallel()

	const goMod = "module github.com/lightninglabs/lndinit\n\n" +
		"go 1.25\n\n" +
		"require github.com/lightningnetwork/lnd v0.20.1-beta\n\n" +
		"replace github.com/lightningnetwork/lnd => ../lnd\n"

	_, err := requiredVersion([]byte(goMod), lndModule)
	if err == nil {
		t.Fatalf("expected an error for a replaced lnd dependency")
	}

	// A replace for some other module is fine, as in lndinit's own
	// go.mod, which replaces google.golang.org/protobuf.
	const other = "module github.com/lightninglabs/lndinit\n\n" +
		"go 1.25\n\n" +
		"require github.com/lightningnetwork/lnd v0.20.1-beta\n\n" +
		"replace google.golang.org/protobuf => " +
		"github.com/lightninglabs/protobuf-go-hex-display " +
		"v1.33.0-hex-display\n"

	version, err := requiredVersion([]byte(other), lndModule)
	if err != nil || version != "v0.20.1-beta" {
		t.Fatalf("got (%q, %v), want v0.20.1-beta", version, err)
	}
}

// TestRemoteTagsRejectsOptions checks that a remote can't be smuggled in as
// a git option.
func TestRemoteTagsRejectsOptions(t *testing.T) {
	t.Parallel()

	marker := filepath.Join(t.TempDir(), "ran")
	for _, remote := range []string{"", "--upload-pack=touch " + marker} {
		_, err := remoteTags(context.Background(), remote)
		if err == nil {
			t.Fatalf("%q: expected an error", remote)
		}
	}

	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("the injected command ran")
	}
}
