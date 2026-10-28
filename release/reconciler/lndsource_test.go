package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeLndTree writes the two files readDBVersions reads into a scratch lnd
// source tree.
func writeLndTree(t *testing.T, channeldb, wtdb string) string {
	t.Helper()

	dir := t.TempDir()
	files := map[string]string{
		filepath.Join("channeldb", "db.go"):               channeldb,
		filepath.Join("watchtower", "wtdb", "version.go"): wtdb,
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		err := os.WriteFile(path, []byte(content), 0644)
		if err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	return dir
}

// TestReadDBVersions checks parsing against source shaped like lnd's.
func TestReadDBVersions(t *testing.T) {
	t.Parallel()

	const channeldb = `package channeldb

var (
	dbVersions = []mandatoryVersion{
		{
			number:    0,
			migration: nil,
		},
		{
			// A comment and odd formatting don't matter.
			number: 34, migration: migration34.Migrate,
		},
		{
			number:    35,
			migration: migration35.Migrate,
		},
	}

	optionalVersions = []optionalVersion{{name: "prune"}}
)
`

	const wtdb = `package wtdb

var towerDBVersions = []version{}

var clientDBVersions = []version{
	{txMigration: migration1.Migrate},
	{txMigration: migration2.Migrate},
	{
		dbMigration: migration3.Migrate(
			migration3.DefaultSessionsPerTx,
		),
	},
}
`

	got, err := readDBVersions(writeLndTree(t, channeldb, wtdb))
	if err != nil {
		t.Fatalf("readDBVersions: %v", err)
	}

	want := dbVersions{Channel: 35, WatchtowerClient: 3}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// TestReadDBVersionsErrors checks that source the parser doesn't understand
// fails instead of yielding a version.
func TestReadDBVersionsErrors(t *testing.T) {
	t.Parallel()

	const wtdb = "package wtdb\n\nvar clientDBVersions = []version{}\n"

	tests := map[string]string{
		"missing variable": "package channeldb\n",
		"computed number": "package channeldb\n\nvar dbVersions = " +
			"[]mandatoryVersion{{number: next()}}\n",
		"not a literal": "package channeldb\n\nvar dbVersions = " +
			"build()\n",
	}

	for name, channeldb := range tests {
		_, err := readDBVersions(writeLndTree(t, channeldb, wtdb))
		if err == nil {
			t.Fatalf("%s: expected an error", name)
		}
	}
}
