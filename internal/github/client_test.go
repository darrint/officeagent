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

func TestListRecentPRs_noOrgs(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/issues" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
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

	prs, err := c.ListRecentPRs(context.Background(), time.Now().AddDate(0, 0, -1), nil)
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
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
			"items": [
				{
					"number": 7,
					"title": "Add feature",
					"html_url": "https://github.com/org/svc/pull/7",
					"repository_url": "https://api.github.com/repos/org/svc",
					"state": "closed",
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

	prs, err := c.ListRecentPRs(context.Background(), time.Now().AddDate(0, 0, -1), nil)
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
	calls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		q := r.URL.Query().Get("q")
		// When orgs are provided, query must NOT contain involves:@me —
		// we want all org activity, not just the authenticated user's PRs.
		if strings.Contains(q, "involves:") {
			t.Errorf("org-scoped query should not contain involves:@me, got: %s", q)
		}
		w.Header().Set("Content-Type", "application/json")
		// Return one unique PR per call with different repo names so dedup is exercised.
		_, _ = fmt.Fprintf(w, `{
			"items": [
				{
					"number": %d,
					"title": "PR in org%d",
					"html_url": "https://github.com/org%d/repo/pull/%d",
					"repository_url": "https://api.github.com/repos/org%d/repo",
					"state": "open",
					"updated_at": "2026-03-10T12:00:00Z",
					"pull_request": {"merged_at": null},
					"user": {"login": "user"}
				}
			]
		}`, calls, calls, calls, calls, calls)
	}))
	defer ts.Close()

	c := NewClient("tok")
	c.SetBaseURL(ts.URL)

	prs, err := c.ListRecentPRs(context.Background(), time.Now().AddDate(0, 0, -1), []string{"orgA", "orgB"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 2 {
		t.Errorf("expected 2 API calls for 2 orgs, got %d", calls)
	}
	if len(prs) != 2 {
		t.Errorf("expected 2 PRs (one per org), got %d", len(prs))
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

	_, err := c.ListRecentPRs(context.Background(), time.Now().AddDate(0, 0, -1), nil)
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
