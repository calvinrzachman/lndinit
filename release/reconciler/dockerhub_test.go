package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

// Paths served by the fake Docker Hub.
const (
	lndTagPath         = "/v2/repositories/lightninglabs/lnd/tags/"
	lndinitTagListPath = "/v2/repositories/lightninglabs/lndinit/tags"
	brokenTagListPath  = "/v2/repositories/lightninglabs/broken/tags"
)

// newHubServer returns a fake Docker Hub that knows one lnd tag and serves an
// lndinit tag listing split across two pages.
func newHubServer(t *testing.T) *dockerHub {
	t.Helper()

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc(lndTagPath, func(w http.ResponseWriter,
		r *http.Request) {

		switch strings.TrimPrefix(r.URL.Path, lndTagPath) {
		case "v0.20.5-beta.rc1":
			fmt.Fprint(w, `{"name":"v0.20.5-beta.rc1"}`)

		case "rate-limited":
			w.WriteHeader(http.StatusTooManyRequests)

		// Both answer 200 without confirming the requested tag.
		case "wrong-name":
			fmt.Fprint(w, `{"name":"something-else"}`)

		case "html":
			fmt.Fprint(w, `<html>maintenance</html>`)

		default:
			http.NotFound(w, r)
		}
	})

	mux.HandleFunc(lndinitTagListPath, func(w http.ResponseWriter,
		r *http.Request) {

		q := r.URL.Query()
		if q.Get("page") == "2" {
			fmt.Fprint(w, `{"next":null,"results":[`+
				`{"name":"b-lnd-v0.20.5-beta.rc1"}]}`)
			return
		}

		if got := q.Get("name"); got != "-lnd-v0.20.5-beta" {
			t.Errorf("unexpected name filter %q", got)
		}
		fmt.Fprintf(w, `{"next":"%s%s?page=2","results":[`+
			`{"name":"a-lnd-v0.20.5-beta"}]}`, srv.URL,
			lndinitTagListPath)
	})

	// A listing that answers 200 with valid JSON but no results field.
	mux.HandleFunc(brokenTagListPath, func(w http.ResponseWriter,
		_ *http.Request) {

		fmt.Fprint(w, `{"message":"maintenance"}`)
	})

	hub, err := newDockerHub(srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("newDockerHub: %v", err)
	}

	return hub
}

// TestDockerHubTagExists checks the found, missing and error cases.
func TestDockerHubTagExists(t *testing.T) {
	t.Parallel()

	hub := newHubServer(t)
	ctx := context.Background()

	ok, err := hub.TagExists(ctx, "lightninglabs/lnd", "v0.20.5-beta.rc1")
	if err != nil || !ok {
		t.Fatalf("existing tag: got (%v, %v)", ok, err)
	}

	ok, err = hub.TagExists(ctx, "lightninglabs/lnd", "v0.99.0-beta")
	if err != nil || ok {
		t.Fatalf("missing tag: got (%v, %v)", ok, err)
	}

	// Anything other than found or not found must surface as an error,
	// never as "missing".
	_, err = hub.TagExists(ctx, "lightninglabs/lnd", "rate-limited")
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("rate limit: expected error, got %v", err)
	}
}

// TestDockerHubTagsContaining checks that listings follow pagination and
// return the raw substring matches for the caller to filter.
func TestDockerHubTagsContaining(t *testing.T) {
	t.Parallel()

	hub := newHubServer(t)

	tags, err := hub.TagsContaining(
		context.Background(), "lightninglabs/lndinit",
		"-lnd-v0.20.5-beta",
	)
	if err != nil {
		t.Fatalf("TagsContaining: %v", err)
	}

	want := []string{"a-lnd-v0.20.5-beta", "b-lnd-v0.20.5-beta.rc1"}
	if !reflect.DeepEqual(tags, want) {
		t.Fatalf("got %v, want %v", tags, want)
	}
}

// TestDockerHubFailsClosed checks that 200 responses which don't carry the
// expected answer are errors. Reading them as "exists" or "no results" would
// let an error page or an API change turn published releases into builds.
func TestDockerHubFailsClosed(t *testing.T) {
	t.Parallel()

	hub := newHubServer(t)
	ctx := context.Background()

	for _, tag := range []string{"wrong-name", "html"} {
		ok, err := hub.TagExists(ctx, "lightninglabs/lnd", tag)
		if err == nil {
			t.Fatalf("%s: expected an error, got exists=%v", tag,
				ok)
		}
	}

	_, err := hub.TagsContaining(ctx, "lightninglabs/broken", "x")
	if err == nil || !strings.Contains(err.Error(), "no results") {
		t.Fatalf("expected missing results error, got %v", err)
	}
}

// TestNewDockerHubURL checks which API endpoints are accepted.
func TestNewDockerHubURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		url string
		ok  bool
	}{
		{url: "https://hub.docker.com", ok: true},
		{url: "https://hub.docker.com/", ok: true},
		{url: "http://127.0.0.1:8080", ok: true},
		{url: "http://localhost:8080", ok: true},
		{url: "http://hub.docker.com", ok: false},
		{url: "ftp://hub.docker.com", ok: false},
		{url: "hub.docker.com", ok: false},
	}

	for _, tc := range tests {
		_, err := newDockerHub(http.DefaultClient, tc.url)
		if (err == nil) != tc.ok {
			t.Fatalf("%s: got err=%v, want ok=%v", tc.url, err,
				tc.ok)
		}
	}
}

// TestDockerHubStaysOnHost checks that neither a pagination link nor a
// redirect can send a request to another host.
func TestDockerHubStaysOnHost(t *testing.T) {
	t.Parallel()

	var otherHit atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			otherHit.Store(true)
			fmt.Fprint(w, `{"name":"x","next":null,"results":[]}`)
		},
	))
	t.Cleanup(other.Close)

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/v2/repositories/a/tags", func(w http.ResponseWriter,
		_ *http.Request) {

		fmt.Fprintf(w, `{"next":"%s/v2/repositories/a/tags?page=2",`+
			`"results":[]}`, other.URL)
	})
	mux.HandleFunc("/v2/repositories/a/tags/x", func(w http.ResponseWriter,
		r *http.Request) {

		http.Redirect(w, r, other.URL+"/v2/repositories/a/tags/x",
			http.StatusFound)
	})

	hub, err := newDockerHub(srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("newDockerHub: %v", err)
	}
	ctx := context.Background()

	_, err = hub.TagsContaining(ctx, "a", "x")
	if err == nil || !strings.Contains(err.Error(), "refusing next") {
		t.Fatalf("expected refused next page, got %v", err)
	}

	_, err = hub.TagExists(ctx, "a", "x")
	if err == nil || !strings.Contains(err.Error(), "refusing redirect") {
		t.Fatalf("expected refused redirect, got %v", err)
	}

	if otherHit.Load() {
		t.Fatalf("a request reached the other host")
	}
}

// TestDockerHubResponseLimit checks that an oversized body is an error
// instead of being read into memory.
func TestDockerHubResponseLimit(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"next":null,"results":[{"name":"`)
			pad := strings.Repeat("a", 64<<10)
			for i := 0; i <= maxResponseBytes/len(pad); i++ {
				fmt.Fprint(w, pad)
			}
			fmt.Fprint(w, `"}]}`)
		},
	))
	t.Cleanup(srv.Close)

	hub, err := newDockerHub(srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("newDockerHub: %v", err)
	}

	_, err = hub.TagsContaining(context.Background(), "a", "x")
	var tooLarge *http.MaxBytesError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("expected a size limit error, got %v", err)
	}
}
