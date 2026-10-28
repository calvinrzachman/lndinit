package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

// dbVersions are the database versions compiled into some lnd code. lndinit's
// migrate-db refuses to run unless the databases it reads are at exactly these
// versions, so the lnd code inside lndinit and the lnd release it is paired
// with must agree on them.
type dbVersions struct {
	// Channel is channeldb.LatestDBVersion().
	Channel uint32

	// WatchtowerClient is wtdb.LatestDBMigrationVersion().
	WatchtowerClient uint32
}

// String formats the versions for a report.
func (v dbVersions) String() string {
	return fmt.Sprintf("channeldb %d, wtclient %d", v.Channel,
		v.WatchtowerClient)
}

// lndSource reads database versions from lnd source fetched through the Go
// module proxy. Results are cached per version for the length of a pass.
type lndSource struct {
	cache map[string]dbVersions
}

func newLndSource() *lndSource {
	return &lndSource{cache: make(map[string]dbVersions)}
}

// DBVersions returns the database versions compiled into lnd at version,
// which may be a tag or a pseudo-version.
func (s *lndSource) DBVersions(ctx context.Context, version string) (
	dbVersions, error) {

	if v, ok := s.cache[version]; ok {
		return v, nil
	}

	dir, err := downloadModule(ctx, lndModule, version)
	if err != nil {
		return dbVersions{}, err
	}

	v, err := readDBVersions(dir)
	if err != nil {
		return dbVersions{}, fmt.Errorf("lnd %s: %w", version, err)
	}

	s.cache[version] = v
	return v, nil
}

// downloadModule fetches module at version into the module cache and returns
// its directory.
func downloadModule(ctx context.Context, module, version string) (string,
	error) {

	var stdout, stderr bytes.Buffer

	// Run outside any module, so the download can't touch a go.mod.
	cmd := exec.CommandContext(
		ctx, "go", "mod", "download", "-json", module+"@"+version,
	)
	cmd.Dir = os.TempDir()
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	var info struct {
		Dir   string
		Error string
	}
	if err := json.Unmarshal(stdout.Bytes(), &info); err != nil {
		return "", fmt.Errorf("go mod download %s@%s: %v: %s", module,
			version, runErr, bytes.TrimSpace(stderr.Bytes()))
	}
	if info.Error != "" || runErr != nil || info.Dir == "" {
		return "", fmt.Errorf("go mod download %s@%s: %s", module,
			version, info.Error)
	}

	return info.Dir, nil
}

// readDBVersions reads the database versions from an lnd source tree.
//
// channeldb.LatestDBVersion() is the highest number in channeldb's dbVersions
// list, and wtdb.LatestDBMigrationVersion() is the length of wtdb's
// clientDBVersions list. Both are read by parsing the Go source rather than
// by pattern matching, so formatting changes in lnd don't affect the result.
func readDBVersions(lndDir string) (dbVersions, error) {
	channel, err := sliceLiteral(
		filepath.Join(lndDir, "channeldb", "db.go"), "dbVersions",
	)
	if err != nil {
		return dbVersions{}, err
	}

	var v dbVersions
	for _, elt := range channel.Elts {
		n, ok := numberField(elt)
		if !ok {
			return dbVersions{}, fmt.Errorf("channeldb " +
				"dbVersions: entry without a literal number")
		}
		if n > v.Channel {
			v.Channel = n
		}
	}
	if v.Channel == 0 {
		return dbVersions{}, fmt.Errorf("channeldb dbVersions is empty")
	}

	client, err := sliceLiteral(
		filepath.Join(lndDir, "watchtower", "wtdb", "version.go"),
		"clientDBVersions",
	)
	if err != nil {
		return dbVersions{}, err
	}
	v.WatchtowerClient = uint32(len(client.Elts))

	return v, nil
}

// sliceLiteral returns the composite literal assigned to the package-level
// variable name in the Go file at path.
func sliceLiteral(path, name string) (*ast.CompositeLit, error) {
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		return nil, err
	}

	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}

		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, ident := range vs.Names {
				if ident.Name != name || i >= len(vs.Values) {
					continue
				}

				lit, ok := vs.Values[i].(*ast.CompositeLit)
				if !ok {
					return nil, fmt.Errorf("%s in %s is "+
						"not a literal", name, path)
				}

				return lit, nil
			}
		}
	}

	return nil, fmt.Errorf("%s not found in %s", name, path)
}

// numberField returns the integer literal of the "number" field in a struct
// literal such as {number: 35, migration: ...}.
func numberField(expr ast.Expr) (uint32, bool) {
	lit, ok := expr.(*ast.CompositeLit)
	if !ok {
		return 0, false
	}

	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "number" {
			continue
		}
		val, ok := kv.Value.(*ast.BasicLit)
		if !ok || val.Kind != token.INT {
			return 0, false
		}

		n, err := strconv.ParseUint(val.Value, 0, 32)
		if err != nil {
			return 0, false
		}

		return uint32(n), true
	}

	return 0, false
}
