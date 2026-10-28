package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

const (
	// defaultGitHubURL is the GitHub REST API endpoint.
	defaultGitHubURL = "https://api.github.com"

	// defaultLndRepo is the GitHub repository lnd releases are published
	// in.
	defaultLndRepo = "lightningnetwork/lnd"
)

// gitHub reads release state from GitHub's REST API.
type gitHub struct {
	*apiClient

	// repo is the owner/name of the repository whose releases are read.
	repo string
}

// newGitHub returns a GitHub client for repo. A non-empty token is sent with
// every request. It is only needed for a higher rate limit, since everything
// read is public.
func newGitHub(client *http.Client, baseURL, repo, token string) (*gitHub,
	error) {

	api, err := newAPIClient(client, baseURL, "GitHub")
	if err != nil {
		return nil, err
	}
	api.headers["Accept"] = "application/vnd.github+json"
	if token != "" {
		api.headers["Authorization"] = "Bearer " + token
	}

	return &gitHub{apiClient: api, repo: repo}, nil
}

// ReleasePublished reports whether the GitHub release for tag has been
// published.
//
// lnd creates each release as a draft when its tag is pushed and publishes it
// once enough developers have signed the release manifest and its own image
// verification has passed. GitHub doesn't return drafts to readers without
// access, so a missing release counts as not published.
func (g *gitHub) ReleasePublished(ctx context.Context, tag string) (bool,
	error) {

	u := fmt.Sprintf("%s/repos/%s/releases/tags/%s", g.baseURL, g.repo,
		url.PathEscape(tag))

	resp, err := g.get(ctx, u)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// Decoded below.

	case http.StatusNotFound:
		return false, nil

	default:
		return false, fmt.Errorf("GET %s: unexpected status %s", u,
			resp.Status)
	}

	var release struct {
		TagName string `json:"tag_name"`
		Draft   bool   `json:"draft"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return false, fmt.Errorf("GET %s: decoding response: %w", u,
			err)
	}

	// As with Docker Hub, a 200 only counts if it is about the tag we
	// asked for.
	if release.TagName != tag {
		return false, fmt.Errorf("GET %s: response names tag %q", u,
			release.TagName)
	}

	return !release.Draft, nil
}
