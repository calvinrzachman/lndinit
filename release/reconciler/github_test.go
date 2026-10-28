package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGitHubReleasePublished checks the published, draft, missing and error
// cases, and that the token is sent.
func TestGitHubReleasePublished(t *testing.T) {
	t.Parallel()

	const prefix = "/repos/lightningnetwork/lnd/releases/tags/"

	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Authorization"); got !=
				"Bearer secret" {

				t.Errorf("unexpected authorization %q", got)
			}

			switch strings.TrimPrefix(r.URL.Path, prefix) {
			case "v0.21.4-beta":
				fmt.Fprint(w, `{"tag_name":"v0.21.4-beta",`+
					`"draft":false}`)

			case "v0.21.5-beta":
				fmt.Fprint(w, `{"tag_name":"v0.21.5-beta",`+
					`"draft":true}`)

			case "wrong-tag":
				fmt.Fprint(w, `{"tag_name":"v0.1.0",`+
					`"draft":false}`)

			case "rate-limited":
				w.WriteHeader(http.StatusForbidden)

			default:
				http.NotFound(w, r)
			}
		},
	))
	t.Cleanup(srv.Close)

	gh, err := newGitHub(srv.Client(), srv.URL, defaultLndRepo, "secret")
	if err != nil {
		t.Fatalf("newGitHub: %v", err)
	}
	ctx := context.Background()

	tests := []struct {
		tag       string
		published bool
		wantErr   bool
	}{
		{tag: "v0.21.4-beta", published: true},
		{tag: "v0.21.5-beta", published: false},
		{tag: "v0.21.6-beta", published: false},
		{tag: "wrong-tag", wantErr: true},
		{tag: "rate-limited", wantErr: true},
	}

	for _, tc := range tests {
		published, err := gh.ReleasePublished(ctx, tc.tag)
		if (err != nil) != tc.wantErr {
			t.Fatalf("%s: got err=%v, want error=%v", tc.tag, err,
				tc.wantErr)
		}
		if published != tc.published {
			t.Fatalf("%s: got published=%v, want %v", tc.tag,
				published, tc.published)
		}
	}
}
