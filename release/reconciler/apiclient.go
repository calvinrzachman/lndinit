package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// maxResponseBytes bounds a single response body. A page of 100 tags or a
// GitHub release with its asset list is well under this.
const maxResponseBytes = 4 << 20

// apiClient issues GET requests to one HTTP API. The base URL must use https,
// except on a loopback host, which tests use.
//
// Requests never leave the base URL's host. Redirects and links pointing
// elsewhere are refused, so a response can't send the reconciler to another
// service, and a credential sent to the API can't follow it there.
type apiClient struct {
	client  *http.Client
	base    *url.URL
	baseURL string

	// headers are set on every request.
	headers map[string]string
}

// newAPIClient returns a client for the API at baseURL. name describes the
// API in error messages.
func newAPIClient(client *http.Client, baseURL, name string) (*apiClient,
	error) {

	base, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("invalid %s URL: %w", name, err)
	}

	switch {
	case base.Host == "":
		return nil, fmt.Errorf("invalid %s URL %q: no host", name,
			baseURL)

	case base.Scheme == "https":
		// Accepted.

	case base.Scheme == "http" && isLoopback(base.Hostname()):
		// Accepted, for tests against a local server.

	default:
		return nil, fmt.Errorf("invalid %s URL %q: must use https",
			name, baseURL)
	}

	a := &apiClient{
		base:    base,
		baseURL: base.String(),
		headers: map[string]string{
			"Accept":     "application/json",
			"User-Agent": "lndinit-release-reconciler",
		},
	}

	c := *client
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !a.sameOrigin(req.URL) {
			return fmt.Errorf("refusing redirect to %s", req.URL)
		}
		if len(via) >= 5 {
			return fmt.Errorf("too many redirects")
		}

		return nil
	}
	a.client = &c

	return a, nil
}

// isLoopback reports whether host names the local machine.
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}

	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// sameOrigin reports whether u has the same scheme and host as the base URL.
func (a *apiClient) sameOrigin(u *url.URL) bool {
	return u.Scheme == a.base.Scheme && u.Host == a.base.Host
}

// get issues a GET request. The body is limited to maxResponseBytes, and the
// body of a non-200 response is drained so the connection can be reused.
func (a *apiClient) get(ctx context.Context, u string) (*http.Response,
	error) {

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range a.headers {
		req.Header.Set(k, v)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", u, err)
	}

	// A nil ResponseWriter is fine here: it is only used to tell a server
	// that a request body was too large.
	resp.Body = http.MaxBytesReader(nil, resp.Body, maxResponseBytes)

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
	}

	return resp, nil
}
