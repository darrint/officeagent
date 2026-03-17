package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// emptyEnrichHandler returns empty arrays for all enrichment endpoints
// (/pulls/.../reviews, /pulls/.../comments, /issues/.../comments,
// /pulls/.../commits) and delegates /search/issues to searchHandler.
func emptyEnrichHandler(searchHandler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case p == "/search/issues":
			searchHandler(w, r)
		case strings.Contains(p, "/reviews"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `[]`)
		case strings.Contains(p, "/comments"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `[]`)
		case strings.Contains(p, "/commits"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `[]`)
		default:
			http.NotFound(w, r)
		}
	}
}

func TestListRecentPRs_noOrgs(t *testing.T) {
	ts := httptest.NewServer(emptyEnrichHandler(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if q == "" {
			t.Error("expected non-empty q parameter")
		}
		// Without orgs configured, query must scope to the authenticated user.
		if !strings.Contains(q, "involves:@me") {
			t.Errorf("no-org query should contain involves:@me, got: %s", q)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("unexpected Authorization: %s", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
			"items": [
				{
					"number": 42,
					"title": "Fix the bug",
					"html_url": "https://github.com/owner/repo/pull/42",
					"repository_url": "https://api.github.com/repos/owner/repo",
					"state": "open",
					"created_at": "2026-03-09T10:00:00Z",
					"updated_at": "2026-03-10T12:00:00Z",
					"pull_request": {"merged_at": null},
					"user": {"login": "alice"}
				}
			]
		}`)
	}))
	defer ts.Close()

	c := NewClient("test-token")
	c.SetBaseURL(ts.URL)

	prs, err := c.ListRecentPRs(context.Background(), time.Now().AddDate(0, 0, -1), nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prs) != 1 {
		t.Fatalf("expected 1 PR, got %d", len(prs))
	}
	pr := prs[0]
	if pr.Number != 42 {
		t.Errorf("Number: got %d, want 42", pr.Number)
	}
	if pr.Repo != "owner/repo" {
		t.Errorf("Repo: got %q, want %q", pr.Repo, "owner/repo")
	}
	if pr.Author != "alice" {
		t.Errorf("Author: got %q, want %q", pr.Author, "alice")
	}
	if pr.State != "open" {
		t.Errorf("State: got %q, want %q", pr.State, "open")
	}
	if pr.MergedAt != nil {
		t.Errorf("MergedAt: expected nil, got %v", pr.MergedAt)
	}
}

func TestListRecentPRs_merged(t *testing.T) {
	ts := httptest.NewServer(emptyEnrichHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
			"items": [
				{
					"number": 7,
					"title": "Add feature",
					"html_url": "https://github.com/org/svc/pull/7",
					"repository_url": "https://api.github.com/repos/org/svc",
					"state": "closed",
					"created_at": "2026-03-10T08:00:00Z",
					"updated_at": "2026-03-11T09:00:00Z",
					"pull_request": {"merged_at": "2026-03-11T08:55:00Z"},
					"user": {"login": "bob"}
				}
			]
		}`)
	}))
	defer ts.Close()

	c := NewClient("tok")
	c.SetBaseURL(ts.URL)

	prs, err := c.ListRecentPRs(context.Background(), time.Now().AddDate(0, 0, -1), nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prs) != 1 {
		t.Fatalf("expected 1 PR, got %d", len(prs))
	}
	if prs[0].MergedAt == nil {
		t.Error("expected MergedAt to be set")
	}
}

func TestListRecentPRs_withOrgs(t *testing.T) {
	searchCalls := 0
	ts := httptest.NewServer(emptyEnrichHandler(func(w http.ResponseWriter, r *http.Request) {
		searchCalls++
		q := r.URL.Query().Get("q")
		// When orgs are provided, query must NOT contain involves:@me.
		if strings.Contains(q, "involves:") {
			t.Errorf("org-scoped query should not contain involves:@me, got: %s", q)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{
			"items": [
				{
					"number": %d,
					"title": "PR in org%d",
					"html_url": "https://github.com/org%d/repo/pull/%d",
					"repository_url": "https://api.github.com/repos/org%d/repo",
					"state": "open",
					"created_at": "2026-03-09T12:00:00Z",
					"updated_at": "2026-03-10T12:00:00Z",
					"pull_request": {"merged_at": null},
					"user": {"login": "user"}
				}
			]
		}`, searchCalls, searchCalls, searchCalls, searchCalls, searchCalls)
	}))
	defer ts.Close()

	c := NewClient("tok")
	c.SetBaseURL(ts.URL)

	prs, err := c.ListRecentPRs(context.Background(), time.Now().AddDate(0, 0, -1), []string{"orgA", "orgB"}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if searchCalls != 2 {
		t.Errorf("expected 2 search API calls for 2 orgs, got %d", searchCalls)
	}
	if len(prs) != 2 {
		t.Errorf("expected 2 PRs (one per org), got %d", len(prs))
	}
}

func TestListRecentPRs_withOrgsAndUsername(t *testing.T) {
	var queries []string
	ts := httptest.NewServer(emptyEnrichHandler(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Query().Get("q"))
		w.Header().Set("Content-Type", "application/json")
		n := len(queries)
		_, _ = fmt.Fprintf(w, `{
			"items": [
				{
					"number": %d,
					"title": "PR %d",
					"html_url": "https://github.com/owner%d/repo/pull/%d",
					"repository_url": "https://api.github.com/repos/owner%d/repo",
					"state": "open",
					"created_at": "2026-03-09T12:00:00Z",
					"updated_at": "2026-03-10T12:00:00Z",
					"pull_request": {"merged_at": null},
					"user": {"login": "user"}
				}
			]
		}`, n, n, n, n, n)
	}))
	defer ts.Close()

	c := NewClient("tok")
	c.SetBaseURL(ts.URL)

	prs, err := c.ListRecentPRs(context.Background(), time.Now().AddDate(0, 0, -1), []string{"myorg"}, "mylogin")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(queries) != 2 {
		t.Fatalf("expected 2 search API calls, got %d", len(queries))
	}
	if !strings.Contains(queries[0], "org:myorg") {
		t.Errorf("first query should contain org:myorg, got: %s", queries[0])
	}
	if !strings.Contains(queries[1], "user:mylogin") {
		t.Errorf("second query should contain user:mylogin, got: %s", queries[1])
	}
	if len(prs) != 2 {
		t.Errorf("expected 2 PRs, got %d", len(prs))
	}
}

func TestListRecentPRs_enrichment(t *testing.T) {
	// Test that reviews, comments, and recent commits are fetched and attached.
	// Use RFC3339 timestamps far in the future relative to cutoff so they pass
	// the 24h filter inside fetchComments / fetchCommits.
	recentTime := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		switch {
		case p == "/search/issues":
			_, _ = fmt.Fprint(w, `{
				"items": [
					{
						"number": 1,
						"title": "Test PR",
						"html_url": "https://github.com/org/repo/pull/1",
						"repository_url": "https://api.github.com/repos/org/repo",
						"state": "open",
						"created_at": "2026-03-09T10:00:00Z",
						"updated_at": "2026-03-10T10:00:00Z",
						"pull_request": {"merged_at": null},
						"user": {"login": "alice"}
					}
				]
			}`)
		case strings.Contains(p, "/reviews"):
			_, _ = fmt.Fprintf(w, `[
				{"state": "APPROVED", "submitted_at": %q, "user": {"login": "bob"}}
			]`, recentTime)
		case strings.HasSuffix(p, "/pulls/1/comments"):
			_, _ = fmt.Fprintf(w, `[
				{"body": "Looks good!", "created_at": %q, "user": {"login": "carol"}}
			]`, recentTime)
		case strings.HasSuffix(p, "/issues/1/comments"):
			_, _ = fmt.Fprint(w, `[]`)
		case strings.Contains(p, "/commits"):
			_, _ = fmt.Fprintf(w, `[
				{
					"sha": "abc1234def",
					"commit": {
						"message": "Fix bug\n\nMore details",
						"author": {"name": "alice", "date": %q}
					}
				}
			]`, recentTime)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	c := NewClient("tok")
	c.SetBaseURL(ts.URL)

	prs, err := c.ListRecentPRs(context.Background(), time.Now().AddDate(0, 0, -30), nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prs) != 1 {
		t.Fatalf("expected 1 PR, got %d", len(prs))
	}
	pr := prs[0]

	if len(pr.Reviews) != 1 {
		t.Errorf("expected 1 review, got %d", len(pr.Reviews))
	} else {
		if pr.Reviews[0].Author != "bob" {
			t.Errorf("review author: got %q, want %q", pr.Reviews[0].Author, "bob")
		}
		if pr.Reviews[0].State != "APPROVED" {
			t.Errorf("review state: got %q, want %q", pr.Reviews[0].State, "APPROVED")
		}
	}

	if len(pr.Comments) != 1 {
		t.Errorf("expected 1 comment, got %d", len(pr.Comments))
	} else if pr.Comments[0].Author != "carol" {
		t.Errorf("comment author: got %q, want %q", pr.Comments[0].Author, "carol")
	}

	if len(pr.RecentCommits) != 1 {
		t.Errorf("expected 1 recent commit, got %d", len(pr.RecentCommits))
	} else {
		if pr.RecentCommits[0].SHA != "abc1234" {
			t.Errorf("commit SHA: got %q, want %q", pr.RecentCommits[0].SHA, "abc1234")
		}
		if pr.RecentCommits[0].Message != "Fix bug" {
			t.Errorf("commit message: got %q, want %q", pr.RecentCommits[0].Message, "Fix bug")
		}
	}
}

func TestListRecentPRs_apiError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"message": "Bad credentials"}`)
	}))
	defer ts.Close()

	c := NewClient("bad-token")
	c.SetBaseURL(ts.URL)

	_, err := c.ListRecentPRs(context.Background(), time.Now().AddDate(0, 0, -1), nil, "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestRepoFromURL(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"https://api.github.com/repos/owner/repo", "owner/repo"},
		{"https://api.github.com/repos/my-org/my-repo", "my-org/my-repo"},
		{"not-a-url", "not-a-url"},
	}
	for _, tt := range tests {
		got := repoFromURL(tt.in)
		if got != tt.want {
			t.Errorf("repoFromURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
