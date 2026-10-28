package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

const (
	// defaultHubURL is the Docker Hub API endpoint.
	defaultHubURL = "https://hub.docker.com"

	// maxTagPages bounds how many result pages a tag listing follows, so
	// a misbehaving API can't keep a pass running forever.
	maxTagPages = 20
)

// dockerHub is a registry backed by Docker Hub's public API. All requests
// are anonymous, since the reconciler only reads.
type dockerHub struct {
	*apiClient
}

// newDockerHub returns a Docker Hub client for the API at baseURL. See
// apiClient for the rules every request follows.
func newDockerHub(client *http.Client, baseURL string) (*dockerHub, error) {
	api, err := newAPIClient(client, baseURL, "hub")
	if err != nil {
		return nil, err
	}

	return &dockerHub{apiClient: api}, nil
}

// A compile-time check that dockerHub implements registry.
var _ registry = (*dockerHub)(nil)

// TagExists reports whether repo:tag exists on Docker Hub. A 200 response
// only counts if its body names the requested tag, so an unexpected page
// served with a 200, such as an error page or a changed API, is an error
// rather than a yes.
func (h *dockerHub) TagExists(ctx context.Context, repo,
	tag string) (bool, error) {

	u := fmt.Sprintf("%s/v2/repositories/%s/tags/%s", h.baseURL, repo,
		url.PathEscape(tag))

	resp, err := h.get(ctx, u)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var body struct {
			Name string `json:"name"`
		}
		err := json.NewDecoder(resp.Body).Decode(&body)
		if err != nil {
			return false, fmt.Errorf("GET %s: decoding "+
				"response: %w", u, err)
		}
		if body.Name != tag {
			return false, fmt.Errorf("GET %s: response names "+
				"tag %q", u, body.Name)
		}

		return true, nil

	case http.StatusNotFound:
		return false, nil
	}

	return false, fmt.Errorf("GET %s: unexpected status %s", u,
		resp.Status)
}

// tagPage is one page of a Docker Hub tag listing.
type tagPage struct {
	Next string `json:"next"`

	// Results is a pointer so that a page without the field can be told
	// apart from a page with no matches.
	Results *[]struct {
		Name string `json:"name"`
	} `json:"results"`
}

// TagsContaining returns the tags in repo whose name contains substr, which
// is how Docker Hub's name filter matches. Filtering server side keeps the
// listing small: the lndinit repository also holds a daily-testing tag for
// every day since the image was first published.
func (h *dockerHub) TagsContaining(ctx context.Context, repo,
	substr string) ([]string, error) {

	q := url.Values{}
	q.Set("name", substr)
	q.Set("page_size", "100")
	next := fmt.Sprintf("%s/v2/repositories/%s/tags?%s", h.baseURL, repo,
		q.Encode())

	var tags []string
	for page := 0; next != ""; page++ {
		if page == maxTagPages {
			return nil, fmt.Errorf("listing %s tags for %q: more "+
				"than %d pages", repo, substr, maxTagPages)
		}

		p, err := h.tagPage(ctx, next)
		if err != nil {
			return nil, err
		}

		for _, r := range *p.Results {
			tags = append(tags, r.Name)
		}

		if p.Next != "" {
			u, err := url.Parse(p.Next)
			if err != nil || !h.sameOrigin(u) {
				return nil, fmt.Errorf("listing %s tags: "+
					"refusing next page %q", repo, p.Next)
			}
		}
		next = p.Next
	}

	return tags, nil
}

// tagPage fetches and decodes a single page of a tag listing.
func (h *dockerHub) tagPage(ctx context.Context, u string) (*tagPage, error) {
	resp, err := h.get(ctx, u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: unexpected status %s", u,
			resp.Status)
	}

	var p tagPage
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return nil, fmt.Errorf("GET %s: decoding response: %w", u, err)
	}

	// A 200 without a result list is not an empty result. Reading it as
	// one would make every image look missing and every release look due
	// for a build.
	if p.Results == nil {
		return nil, fmt.Errorf("GET %s: response has no results", u)
	}

	return &p, nil
}
