package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// newTestServer starts an https server with the given routes and returns it
// with a client that trusts it.
func newTestServer(t *testing.T,
	routes map[string]http.HandlerFunc) (*httptest.Server, *http.Client) {

	t.Helper()

	mux := http.NewServeMux()
	for pattern, h := range routes {
		mux.HandleFunc(pattern, h)
	}
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)

	return srv, newHTTPClient(srv.Client())
}

// reply returns a handler that writes body as JSON.
func reply(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, body)
	}
}

// TestTagExists checks that only a 200 naming the tag counts as a yes.
func TestTagExists(t *testing.T) {
	srv, client := newTestServer(t, map[string]http.HandlerFunc{
		"/v2/repositories/r/tags/ok":    reply(`{"name":"ok"}`),
		"/v2/repositories/r/tags/wrong": reply(`{"name":"other"}`),
		"/v2/repositories/r/tags/moved": func(w http.ResponseWriter,
			r *http.Request) {

			http.Redirect(w, r, "https://example.com/",
				http.StatusFound)
		},
		"/v2/repositories/r/tags/garbled": reply(`<html>`),
	})
	hub := &dockerHub{client: client, baseURL: srv.URL}
	ctx := context.Background()

	if ok, err := hub.TagExists(ctx, "r", "ok"); !ok || err != nil {
		t.Errorf("ok: got %v, %v", ok, err)
	}
	if ok, err := hub.TagExists(ctx, "r", "missing"); ok || err != nil {
		t.Errorf("missing: got %v, %v", ok, err)
	}
	for _, tag := range []string{"wrong", "moved", "garbled"} {
		if _, err := hub.TagExists(ctx, "r", tag); err == nil {
			t.Errorf("%s: expected an error", tag)
		}
	}
}

// TestTagsContaining checks paging and the answers that must fail the pass.
func TestTagsContaining(t *testing.T) {
	var srv *httptest.Server
	srv, client := newTestServer(t, map[string]http.HandlerFunc{
		"/v2/repositories/paged/tags": func(w http.ResponseWriter,
			r *http.Request) {

			if r.URL.Query().Get("page") == "2" {
				fmt.Fprint(w, `{"results":[{"name":"b"}]}`)
				return
			}
			fmt.Fprintf(w, `{"next":"%s%s?page=2",`+
				`"results":[{"name":"a"}]}`, srv.URL,
				r.URL.Path)
		},
		"/v2/repositories/empty/tags":     reply(`{"results":[]}`),
		"/v2/repositories/noresults/tags": reply(`{}`),
		"/v2/repositories/offsite/tags": reply(
			`{"next":"https://example.com/x","results":[]}`,
		),
	})
	hub := &dockerHub{client: client, baseURL: srv.URL}
	ctx := context.Background()

	got, err := hub.TagsContaining(ctx, "paged", "x")
	if err != nil || !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("paged: got %v, %v", got, err)
	}

	got, err = hub.TagsContaining(ctx, "empty", "x")
	if err != nil || len(got) != 0 {
		t.Errorf("empty: got %v, %v", got, err)
	}

	for _, repo := range []string{"noresults", "offsite", "missing"} {
		if _, err := hub.TagsContaining(ctx, repo, "x"); err == nil {
			t.Errorf("%s: expected an error", repo)
		}
	}
}

// TestReleasePublished checks how lnd's GitHub release state is read.
func TestReleasePublished(t *testing.T) {
	srv, client := newTestServer(t, map[string]http.HandlerFunc{
		"/repos/o/r/releases/tags/pub": reply(
			`{"tag_name":"pub","draft":false}`,
		),
		"/repos/o/r/releases/tags/draft": reply(
			`{"tag_name":"draft","draft":true}`,
		),
		"/repos/o/r/releases/tags/nodraft": reply(
			`{"tag_name":"nodraft"}`,
		),
		"/repos/o/r/releases/tags/wrong": reply(
			`{"tag_name":"other","draft":false}`,
		),
	})
	gh := &gitHub{client: client, baseURL: srv.URL, repo: "o/r"}
	ctx := context.Background()

	tests := map[string]bool{"pub": true, "draft": false, "missing": false}
	for tag, want := range tests {
		got, err := gh.ReleasePublished(ctx, tag)
		if err != nil || got != want {
			t.Errorf("%s: got %v, %v, want %v", tag, got, err, want)
		}
	}

	for _, tag := range []string{"nodraft", "wrong"} {
		if _, err := gh.ReleasePublished(ctx, tag); err == nil {
			t.Errorf("%s: expected an error", tag)
		}
	}
}

// TestGetJSONRefusesHTTP checks that plain http is never used.
func TestGetJSONRefusesHTTP(t *testing.T) {
	var v any
	_, err := getJSON(
		context.Background(), newHTTPClient(&http.Client{}),
		"http://example.com/", nil, &v,
	)
	if err == nil {
		t.Fatal("expected an error")
	}
}
