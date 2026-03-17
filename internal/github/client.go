package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const apiBase = "https://api.github.com"

// Review represents a pull request review (approval, rejection, or comment).
type Review struct {
	Author    string
	State     string // "APPROVED", "CHANGES_REQUESTED", "COMMENTED", etc.
	CreatedAt time.Time
}

// Comment represents a single comment on a pull request (review comment or
// general issue comment).
type Comment struct {
	Author    string
	Body      string // first 200 chars
	CreatedAt time.Time
}

// Commit represents a commit pushed to a pull request.
type Commit struct {
	Author    string
	SHA       string // short (7 chars)
	Message   string // first line only
	CreatedAt time.Time
}

// PullRequest is a simplified representation of a GitHub pull request returned
// by the search API.
type PullRequest struct {
	Number        int
	Title         string
	HTMLURL       string
	Repo          string // "owner/repo"
	State         string // "open" or "closed"
	CreatedAt     time.Time
	UpdatedAt     time.Time
	MergedAt      *time.Time
	Author        string
	Reviews       []Review
	Comments      []Comment
	RecentCommits []Commit
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
	CreatedAt     time.Time `json:"created_at"`
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

// ListRecentPRs returns pull requests updated since `since`, enriched with
// reviews, comments, and recent commits from the last 24 hours.
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
		return c.searchAndEnrich(ctx, since, fmt.Sprintf("type:pr involves:@me updated:>%s", dateStr))
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
		prs, err := c.searchAndEnrich(ctx, since, fmt.Sprintf("type:pr org:%s updated:>%s", org, dateStr))
		if err != nil {
			return nil, fmt.Errorf("search org %s: %w", org, err)
		}
		addPRs(prs)
	}

	if username != "" {
		prs, err := c.searchAndEnrich(ctx, since, fmt.Sprintf("type:pr user:%s updated:>%s", username, dateStr))
		if err != nil {
			return nil, fmt.Errorf("search user %s: %w", username, err)
		}
		addPRs(prs)
	}

	return all, nil
}

// searchAndEnrich runs a search query and then concurrently fetches reviews,
// comments, and recent commits for each PR.
func (c *Client) searchAndEnrich(ctx context.Context, since time.Time, query string) ([]PullRequest, error) {
	prs, err := c.search(ctx, query)
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().UTC().Add(-24 * time.Hour)

	// Enrich up to 20 PRs concurrently to avoid hammering the API.
	const maxConcurrent = 5
	sem := make(chan struct{}, maxConcurrent)
	var mu sync.Mutex
	var wg sync.WaitGroup

	for i := range prs {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			pr := &prs[idx]
			reviews, _ := c.fetchReviews(ctx, pr.Repo, pr.Number)
			comments, _ := c.fetchComments(ctx, pr.Repo, pr.Number, cutoff)
			commits, _ := c.fetchCommits(ctx, pr.Repo, pr.Number, cutoff)

			mu.Lock()
			pr.Reviews = reviews
			pr.Comments = comments
			pr.RecentCommits = commits
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	_ = since // kept for call-site clarity
	return prs, nil
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
			CreatedAt: item.CreatedAt,
			UpdatedAt: item.UpdatedAt,
			MergedAt:  item.PullRequest.MergedAt,
			Author:    item.User.Login,
		}
	}
	return prs, nil
}

// fetchReviews fetches all reviews for a PR.
func (c *Client) fetchReviews(ctx context.Context, repo string, number int) ([]Review, error) {
	url := fmt.Sprintf("%s/repos/%s/pulls/%d/reviews?per_page=100", c.baseURL, repo, number)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	c.setHeaders(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("reviews %s: %s", repo, resp.Status)
	}

	var items []struct {
		State     string    `json:"state"`
		CreatedAt time.Time `json:"submitted_at"`
		User      struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return nil, err
	}
	reviews := make([]Review, len(items))
	for i, it := range items {
		reviews[i] = Review{
			Author:    it.User.Login,
			State:     it.State,
			CreatedAt: it.CreatedAt,
		}
	}
	return reviews, nil
}

// fetchComments fetches review comments and issue comments for a PR created
// after cutoff.
func (c *Client) fetchComments(ctx context.Context, repo string, number int, cutoff time.Time) ([]Comment, error) {
	var all []Comment

	// Review (inline diff) comments.
	reviewURL := fmt.Sprintf("%s/repos/%s/pulls/%d/comments?per_page=100", c.baseURL, repo, number)
	rc, err := c.fetchCommentList(ctx, reviewURL, cutoff)
	if err == nil {
		all = append(all, rc...)
	}

	// General issue comments.
	issueURL := fmt.Sprintf("%s/repos/%s/issues/%d/comments?per_page=100", c.baseURL, repo, number)
	ic, err := c.fetchCommentList(ctx, issueURL, cutoff)
	if err == nil {
		all = append(all, ic...)
	}

	return all, nil
}

func (c *Client) fetchCommentList(ctx context.Context, rawURL string, cutoff time.Time) ([]Comment, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	c.setHeaders(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("comments %s: %s", rawURL, resp.Status)
	}

	var items []struct {
		Body      string    `json:"body"`
		CreatedAt time.Time `json:"created_at"`
		User      struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return nil, err
	}

	var comments []Comment
	for _, it := range items {
		if it.CreatedAt.Before(cutoff) {
			continue
		}
		body := it.Body
		if len(body) > 200 {
			body = body[:200]
		}
		comments = append(comments, Comment{
			Author:    it.User.Login,
			Body:      body,
			CreatedAt: it.CreatedAt,
		})
	}
	return comments, nil
}

// fetchCommits fetches commits pushed to a PR after cutoff.
func (c *Client) fetchCommits(ctx context.Context, repo string, number int, cutoff time.Time) ([]Commit, error) {
	rawURL := fmt.Sprintf("%s/repos/%s/pulls/%d/commits?per_page=100", c.baseURL, repo, number)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	c.setHeaders(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("commits %s: %s", repo, resp.Status)
	}

	var items []struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message string `json:"message"`
			Author  struct {
				Name string    `json:"name"`
				Date time.Time `json:"date"`
			} `json:"author"`
		} `json:"commit"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return nil, err
	}

	var commits []Commit
	for _, it := range items {
		if it.Commit.Author.Date.Before(cutoff) {
			continue
		}
		msg := it.Commit.Message
		if idx := strings.IndexByte(msg, '\n'); idx >= 0 {
			msg = msg[:idx]
		}
		sha := it.SHA
		if len(sha) > 7 {
			sha = sha[:7]
		}
		commits = append(commits, Commit{
			Author:    it.Commit.Author.Name,
			SHA:       sha,
			Message:   msg,
			CreatedAt: it.Commit.Author.Date,
		})
	}
	return commits, nil
}

func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
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
