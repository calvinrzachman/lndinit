package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const (
	// hubURL is the Docker Hub API endpoint.
	hubURL = "https://hub.docker.com"

	// gitHubURL is the GitHub REST API endpoint.
	gitHubURL = "https://api.github.com"

	// maxBodyBytes bounds a response body. A page of 100 tags or a GitHub
	// release with its asset list is well under this.
	maxBodyBytes = 4 << 20

	// maxTagPages bounds how many result pages a tag search follows.
	maxTagPages = 20
)

// newHTTPClient returns a copy of base that times out and doesn't follow
// redirects, so every request stays on the host it was sent to. A redirect
// comes back as an unexpected status.
func newHTTPClient(base *http.Client) *http.Client {
	c := *base
	c.Timeout = 30 * time.Second
	c.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	return &c
}

// getJSON GETs u over https and decodes a 200 response into v. It returns
// false without decoding for a 404, and an error for any other status.
func getJSON(ctx context.Context, client *http.Client, u string,
	header http.Header, v any) (bool, error) {

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, err
	}
	if req.URL.Scheme != "https" {
		return false, fmt.Errorf("refusing non-https URL %s", u)
	}
	for k, vs := range header {
		req.Header[k] = vs
	}

	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("GET %s: %w", u, err)
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

	// A body cut off at the limit fails to decode, so it is an error too.
	body := io.LimitReader(resp.Body, maxBodyBytes)
	if err := json.NewDecoder(body).Decode(v); err != nil {
		return false, fmt.Errorf("GET %s: decoding response: %w", u,
			err)
	}

	return true, nil
}

// dockerHub reads image tags from Docker Hub's public API anonymously.
type dockerHub struct {
	client  *http.Client
	baseURL string
}

// TagExists reports whether repo:tag exists. A 200 only counts if its body
// names the requested tag, so an unexpected page is an error rather than a
// yes.
func (h *dockerHub) TagExists(ctx context.Context, repo,
	tag string) (bool, error) {

	u := fmt.Sprintf("%s/v2/repositories/%s/tags/%s", h.baseURL, repo,
		url.PathEscape(tag))

	var body struct {
		Name string `json:"name"`
	}
	found, err := getJSON(ctx, h.client, u, nil, &body)
	if err != nil || !found {
		return false, err
	}
	if body.Name != tag {
		return false, fmt.Errorf("GET %s: response names tag %q", u,
			body.Name)
	}

	return true, nil
}

// TagsContaining returns the tags in repo whose name contains substr.
// Filtering on the server keeps the listing small, since the lndinit
// repository also holds a daily-testing tag for every day.
func (h *dockerHub) TagsContaining(ctx context.Context, repo,
	substr string) ([]string, error) {

	base, err := url.Parse(h.baseURL)
	if err != nil {
		return nil, err
	}

	q := url.Values{"name": {substr}, "page_size": {"100"}}
	next := fmt.Sprintf("%s/v2/repositories/%s/tags?%s", h.baseURL, repo,
		q.Encode())

	var tags []string
	for page := 0; next != ""; page++ {
		if page == maxTagPages {
			return nil, fmt.Errorf("listing %s tags for %q: more "+
				"than %d pages", repo, substr, maxTagPages)
		}

		// Results is a pointer so that a page without it is an error.
		// Reading it as an empty result would make every image look
		// missing.
		var p struct {
			Next    string `json:"next"`
			Results *[]struct {
				Name string `json:"name"`
			} `json:"results"`
		}
		found, err := getJSON(ctx, h.client, next, nil, &p)
		switch {
		case err != nil:
			return nil, err

		case !found:
			return nil, fmt.Errorf("GET %s: not found", next)

		case p.Results == nil:
			return nil, fmt.Errorf("GET %s: response has no "+
				"results", next)
		}

		for _, r := range *p.Results {
			tags = append(tags, r.Name)
		}

		if p.Next != "" {
			u, err := url.Parse(p.Next)
			if err != nil || u.Host != base.Host {
				return nil, fmt.Errorf("listing %s tags: "+
					"refusing next page %q", repo, p.Next)
			}
		}
		next = p.Next
	}

	return tags, nil
}

// gitHub reads lnd's release state from GitHub's REST API.
type gitHub struct {
	client  *http.Client
	baseURL string

	// repo is the owner/name of the repository whose releases are read.
	repo string

	// token, if set, only raises the rate limit. Everything read is
	// public.
	token string
}

// ReleasePublished reports whether the GitHub release for tag has been
// published.
//
// lnd creates each release as a draft and publishes it once enough
// developers have signed it. GitHub doesn't show drafts to readers without
// access, so a missing release counts as not published.
func (g *gitHub) ReleasePublished(ctx context.Context, tag string) (bool,
	error) {

	u := fmt.Sprintf("%s/repos/%s/releases/tags/%s", g.baseURL, g.repo,
		url.PathEscape(tag))

	header := http.Header{"Accept": {"application/vnd.github+json"}}
	if g.token != "" {
		header.Set("Authorization", "Bearer "+g.token)
	}

	// Draft is a pointer so that a response without it is an error
	// rather than a published release.
	var release struct {
		TagName string `json:"tag_name"`
		Draft   *bool  `json:"draft"`
	}
	found, err := getJSON(ctx, g.client, u, header, &release)
	if err != nil || !found {
		return false, err
	}
	if release.TagName != tag || release.Draft == nil {
		return false, fmt.Errorf("GET %s: unexpected response for "+
			"tag %q", u, release.TagName)
	}

	return !*release.Draft, nil
}
