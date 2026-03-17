package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const apiBase = "https://api.github.com"

// PullRequest is a simplified representation of a GitHub pull request returned
// by the search API.
type PullRequest struct {
	Number    int
	Title     string
	HTMLURL   string
	Repo      string // "owner/repo"
	State     string // "open" or "closed"
	UpdatedAt time.Time
	MergedAt  *time.Time
	Author    string
}

// Client makes authenticated requests to the GitHub REST API.
type Client struct {
	token   string
	http    *http.Client
	baseURL string // defaults to apiBase; overridable in tests
}

// NewClient creates a GitHub API client using the given OAuth or personal access token.
func NewClient(token string) *Client {
	return &Client{
		token:   token,
		http:    &http.Client{Timeout: 30 * time.Second},
		baseURL: apiBase,
	}
}

// SetBaseURL overrides the API base URL. Intended for testing only.
func (c *Client) SetBaseURL(u string) { c.baseURL = u }

// searchItem is the raw JSON shape returned by GitHub's search API for a PR item.
type searchItem struct {
	Number        int       `json:"number"`
	Title         string    `json:"title"`
	HTMLURL       string    `json:"html_url"`
	RepositoryURL string    `json:"repository_url"`
	State         string    `json:"state"`
	UpdatedAt     time.Time `json:"updated_at"`
	PullRequest   struct {
		MergedAt *time.Time `json:"merged_at"`
	} `json:"pull_request"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
}

type searchResponse struct {
	Items []searchItem `json:"items"`
}

// ListRecentPRs returns pull requests updated since `since`.
//
// When orgs is non-empty, one query per org is issued for ALL pull requests in
// those orgs (not filtered to the authenticated user), giving a full picture of
// team activity. When username is non-empty, an additional user:<login> query
// is issued so personal repos are always included alongside org results. When
// both orgs and username are empty, the query falls back to PRs involving the
// authenticated user (involves:@me) since searching all of GitHub is not useful.
func (c *Client) ListRecentPRs(ctx context.Context, since time.Time, orgs []string, username string) ([]PullRequest, error) {
	dateStr := since.UTC().Format("2006-01-02")
	if len(orgs) == 0 && username == "" {
		// No org filter and no username: scope to the authenticated user's activity.
		return c.search(ctx, fmt.Sprintf("type:pr involves:@me updated:>%s", dateStr))
	}
	var all []PullRequest
	seen := make(map[string]bool)

	addPRs := func(prs []PullRequest) {
		for _, pr := range prs {
			key := fmt.Sprintf("%s#%d", pr.Repo, pr.Number)
			if !seen[key] {
				seen[key] = true
				all = append(all, pr)
			}
		}
	}

	for _, org := range orgs {
		// Search all PRs in the org — not filtered to involves:@me — so the user
		// gets a full org pulse, not just their own activity.
		prs, err := c.search(ctx, fmt.Sprintf("type:pr org:%s updated:>%s", org, dateStr))
		if err != nil {
			return nil, fmt.Errorf("search org %s: %w", org, err)
		}
		addPRs(prs)
	}

	if username != "" {
		prs, err := c.search(ctx, fmt.Sprintf("type:pr user:%s updated:>%s", username, dateStr))
		if err != nil {
			return nil, fmt.Errorf("search user %s: %w", username, err)
		}
		addPRs(prs)
	}

	return all, nil
}

func (c *Client) search(ctx context.Context, query string) ([]PullRequest, error) {
	params := url.Values{
		"per_page": {"50"},
		"sort":     {"updated"},
		"order":    {"desc"},
		"q":        {query},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/search/issues?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github search: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		var errBody struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&errBody)
		return nil, fmt.Errorf("github %s: %s", resp.Status, errBody.Message)
	}

	var sr searchResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return nil, fmt.Errorf("decode search response: %w", err)
	}

	prs := make([]PullRequest, len(sr.Items))
	for i, item := range sr.Items {
		prs[i] = PullRequest{
			Number:    item.Number,
			Title:     item.Title,
			HTMLURL:   item.HTMLURL,
			Repo:      repoFromURL(item.RepositoryURL),
			State:     item.State,
			UpdatedAt: item.UpdatedAt,
			MergedAt:  item.PullRequest.MergedAt,
			Author:    item.User.Login,
		}
	}
	return prs, nil
}

// repoFromURL extracts "owner/repo" from a GitHub repository URL of the form
// "https://api.github.com/repos/owner/repo".
func repoFromURL(rawURL string) string {
	const marker = "/repos/"
	idx := strings.LastIndex(rawURL, marker)
	if idx < 0 {
		return rawURL
	}
	return rawURL[idx+len(marker):]
}
