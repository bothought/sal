package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// release is the part of a GitHub release the upgrade needs. Everything else
// the API returns is ignored.
type release struct {
	Tag    string
	Page   string
	Assets []releaseAsset
}

type releaseAsset struct {
	Name string
	URL  string
	Size int64
}

// fetchRelease asks GitHub for the latest published, non-draft, non-prerelease
// release of the project repository. Failures are named by kind, because the
// kind decides the message and whether waiting is worth suggesting.
func fetchRelease(ctx context.Context, o options) (*release, error) {
	endpoint := strings.TrimRight(o.APIBase, "/") + "/repos/" + RepoOwner + "/" + RepoName + "/releases/latest"
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("cannot build the release URL %q: %w", endpoint, err)
	}
	if !secureURL(u) {
		return nil, fmt.Errorf("refusing to fetch releases over %s", u.Scheme)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("cannot build the release request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", userAgent(o.Version))

	resp, err := o.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the release endpoint: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusForbidden, http.StatusTooManyRequests:
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("the GitHub API rate limit was reached (HTTP %d); try again later", resp.StatusCode)
	case http.StatusNotFound:
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("no release found for %s/%s", RepoOwner, RepoName)
	default:
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("the release endpoint answered HTTP %d: %s", resp.StatusCode, truncate(strings.TrimSpace(string(snippet)), 200))
	}

	var body struct {
		TagName    string `json:"tag_name"`
		HTMLURL    string `json:"html_url"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
		Assets     []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
			Size int64  `json:"size"`
		} `json:"assets"`
	}
	// A release body is small; the cap is there so a wrong endpoint cannot
	// stream forever into memory.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("cannot read the release description: %w", err)
	}
	if body.TagName == "" {
		return nil, fmt.Errorf("the release description carries no tag name")
	}
	if body.Draft || body.Prerelease {
		return nil, fmt.Errorf("the latest release %s is a draft or prerelease; refusing to use it", body.TagName)
	}

	rel := &release{Tag: body.TagName, Page: body.HTMLURL}
	for _, a := range body.Assets {
		rel.Assets = append(rel.Assets, releaseAsset{Name: a.Name, URL: a.URL, Size: a.Size})
	}
	return rel, nil
}

// secureURL reports whether a URL may be fetched. Plain HTTP is allowed only
// for a loopback host, which is how the tests reach their local server; every
// real download is HTTPS.
func secureURL(u *url.URL) bool {
	switch strings.ToLower(u.Scheme) {
	case "https":
		return true
	case "http":
		host := strings.ToLower(u.Hostname())
		return host == "localhost" || host == "127.0.0.1" || host == "::1"
	}
	return false
}

func userAgent(version string) string {
	return "sal/" + version
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
