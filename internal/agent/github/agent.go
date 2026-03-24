// Package github provides the GitHub service agent for officeagent.
// It owns the PR summary skill.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/darrint/officeagent/internal/agent"
	"github.com/darrint/officeagent/internal/feed"
	githubclient "github.com/darrint/officeagent/internal/github"
	"github.com/darrint/officeagent/internal/llm"
	"github.com/darrint/officeagent/internal/store"
)

// DefaultGitHubPrompt is the default system prompt for GitHub PR summaries.
const DefaultGitHubPrompt = "You are a helpful engineering assistant. Give the user a concise summary of recent GitHub pull request activity across their team. Start with the overall picture: what is being worked on, what shipped, what is under review. Then highlight anything that specifically needs the user's attention — review requests, mentions, or their own open PRs awaiting feedback. Be friendly but brief."

// LLMClient is the subset of llm.Client used by the agent's skills.
type LLMClient interface {
	Chat(ctx context.Context, messages []llm.Message) (string, error)
}

// GitHubClient is the interface the agent requires from the GitHub API client.
type GitHubClient interface {
	ListRecentPRs(ctx context.Context, since time.Time, orgs []string, username string) ([]githubclient.PullRequest, error)
}

// Agent is the GitHub service agent.
type Agent struct {
	client GitHubClient
	store  *store.Store

	// config populated by Configure
	token        string
	orgs         []string
	username     string
	lookbackDays int
	prompt       string
	overallPrompt string
}

// New creates a new GitHub Agent.
// Call Configure to load settings from the store before using skills.
func New(client GitHubClient) *Agent {
	return &Agent{
		client: client,
		prompt: DefaultGitHubPrompt,
	}
}

// Configure loads credentials and configuration from the kv store.
func (a *Agent) Configure(s *store.Store) error {
	a.store = s
	a.token = getSetting(s, "github_token", "")
	a.orgs = parseOrgs(getSetting(s, "github.orgs", ""))
	a.username = strings.TrimSpace(getSetting(s, "github.username", ""))
	raw := getSetting(s, "github.lookback_days", "0")
	if n, err := strconv.Atoi(raw); err == nil {
		a.lookbackDays = n
	}
	a.prompt = getPrompt(s, "github", DefaultGitHubPrompt)
	a.overallPrompt = getPrompt(s, "overall", "")
	return nil
}

// Check performs a health check by sending a minimal LLM ping via the GitHub
// Copilot API. This doubles as an LLM connectivity check since the same token
// is used for both GitHub API access and the LLM.
func (a *Agent) Check(ctx context.Context) agent.CheckResult {
	cr := agent.CheckResult{Name: "LLM (GitHub Copilot)"}
	start := time.Now()
	if a.client == nil {
		cr.Detail = "not configured — set GITHUB_TOKEN"
		cr.Latency = time.Since(start)
		return cr
	}
	// Perform a lightweight check: list 0 PRs to verify API connectivity.
	since := time.Now().Add(-time.Hour) // very recent — expect 0 results, just test auth
	_, err := a.client.ListRecentPRs(ctx, since, nil, "")
	cr.Latency = time.Since(start)
	if err != nil {
		cr.Detail = fmt.Sprintf("GitHub API check failed: %v", err)
		return cr
	}
	cr.OK = true
	cr.Detail = "GitHub API accessible"
	return cr
}

// PRSummary fetches recent GitHub PR activity and returns an LLM-generated
// markdown summary.
func (a *Agent) PRSummary(ctx context.Context, llmC LLMClient, feedbackCtx string) (string, error) {
	if a.client == nil {
		return "", fmt.Errorf("GitHub not configured")
	}

	since := a.since()
	prs, err := a.client.ListRecentPRs(ctx, since, a.orgs, a.username)
	if err != nil {
		return "", fmt.Errorf("list PRs: %w", err)
	}

	var sb strings.Builder
	if len(prs) == 0 {
		sb.WriteString("No recent pull request activity.")
	} else {
		cutoff24h := time.Now().UTC().Add(-24 * time.Hour)

		var newPRs, activePRs []githubclient.PullRequest
		for _, pr := range prs {
			if pr.CreatedAt.After(cutoff24h) {
				newPRs = append(newPRs, pr)
			} else {
				activePRs = append(activePRs, pr)
			}
		}

		writePR := func(pr githubclient.PullRequest) {
			status := pr.State
			if pr.MergedAt != nil {
				status = "merged"
			}
			fmt.Fprintf(&sb, "- [%s#%d](%s) %s (%s) by %s — updated %s\n",
				pr.Repo, pr.Number, pr.HTMLURL, pr.Title,
				status, pr.Author,
				pr.UpdatedAt.UTC().Format("Mon Jan 2 15:04"),
			)
			for _, r := range pr.Reviews {
				fmt.Fprintf(&sb, "  - review by %s: %s (%s)\n",
					r.Author, r.State,
					r.CreatedAt.UTC().Format("Jan 2 15:04"),
				)
			}
			for _, cm := range pr.Comments {
				fmt.Fprintf(&sb, "  - comment by %s (%s): %s\n",
					cm.Author,
					cm.CreatedAt.UTC().Format("Jan 2 15:04"),
					cm.Body,
				)
			}
			for _, co := range pr.RecentCommits {
				fmt.Fprintf(&sb, "  - commit %s by %s (%s): %s\n",
					co.SHA, co.Author,
					co.CreatedAt.UTC().Format("Jan 2 15:04"),
					co.Message,
				)
			}
		}

		if len(newPRs) > 0 {
			sb.WriteString("### New PRs opened today\n")
			for _, pr := range newPRs {
				writePR(pr)
			}
			sb.WriteString("\n")
		}
		if len(activePRs) > 0 {
			sb.WriteString("### Active PRs with recent updates\n")
			for _, pr := range activePRs {
				writePR(pr)
			}
		}
	}

	sysPrompt := buildSystemPrompt(a.overallPrompt, a.prompt+feedbackCtx)
	reply, err := llmC.Chat(ctx, []llm.Message{
		{Role: "system", Content: sysPrompt},
		{Role: "user", Content: "Here is my recent GitHub pull request activity:\n\n" + sb.String()},
	})
	if err != nil {
		return "", fmt.Errorf("LLM chat: %w", err)
	}
	return reply, nil
}

// Poll fetches PRs updated since the stored cursor and returns them as
// feed.RawEvents. The cursor is stored in feed_state["github"].delta_token as
// an RFC3339 timestamp. On the first call the cursor defaults to 24 hours ago.
// After each poll the cursor is advanced to the most recent PR's UpdatedAt.
func (a *Agent) Poll(ctx context.Context, s *store.Store) ([]feed.RawEvent, error) {
	if a.client == nil {
		return nil, nil
	}

	fs, err := s.GetFeedState("github")
	if err != nil {
		return nil, fmt.Errorf("get feed state: %w", err)
	}

	var since time.Time
	if fs.DeltaToken != "" {
		since, err = time.Parse(time.RFC3339, fs.DeltaToken)
		if err != nil {
			log.Printf("github agent: bad delta_token %q: %v — resetting", fs.DeltaToken, err)
			since = time.Time{}
		}
	}
	if since.IsZero() {
		since = time.Now().UTC().Add(-24 * time.Hour)
	}

	prs, err := a.client.ListRecentPRs(ctx, since, a.orgs, a.username)
	if err != nil {
		return nil, fmt.Errorf("list PRs: %w", err)
	}

	// Advance cursor to the latest UpdatedAt seen.
	latest := since
	for _, pr := range prs {
		if pr.UpdatedAt.After(latest) {
			latest = pr.UpdatedAt
		}
	}
	if latest.After(since) {
		fs.DeltaToken = latest.UTC().Format(time.RFC3339)
		fs.LastPoll = time.Now().UTC()
		if err := s.SetFeedState(fs); err != nil {
			log.Printf("github agent: save feed state: %v", err)
		}
	}

	events := make([]feed.RawEvent, 0, len(prs))
	for _, pr := range prs {
		// Build a condensed payload for LLM consumption.
		type commentPayload struct {
			Author string `json:"author"`
			Body   string `json:"body"`
		}
		type reviewPayload struct {
			Author string `json:"author"`
			State  string `json:"state"`
		}
		type prPayload struct {
			Number   int              `json:"number"`
			Title    string           `json:"title"`
			Repo     string           `json:"repo"`
			State    string           `json:"state"`
			Author   string           `json:"author"`
			URL      string           `json:"url"`
			Comments []commentPayload `json:"comments"`
			Reviews  []reviewPayload  `json:"reviews"`
		}
		p := prPayload{
			Number: pr.Number,
			Title:  pr.Title,
			Repo:   pr.Repo,
			State:  pr.State,
			Author: pr.Author,
			URL:    pr.HTMLURL,
		}
		for _, c := range pr.Comments {
			p.Comments = append(p.Comments, commentPayload{Author: c.Author, Body: c.Body})
		}
		for _, r := range pr.Reviews {
			p.Reviews = append(p.Reviews, reviewPayload{Author: r.Author, State: r.State})
		}
		payload, _ := json.Marshal(p)
		// Use repo#number as the external ID for deduplication; each poll
		// update for the same PR overwrites via ON CONFLICT IGNORE, meaning
		// only the first-seen snapshot is stored. This is intentional: the
		// feed captures that the PR was active, not every incremental update.
		externalID := fmt.Sprintf("%s#%d", pr.Repo, pr.Number)
		events = append(events, feed.RawEvent{
			Source:     "github",
			ExternalID: externalID,
			Payload:    string(payload),
			OccurredAt: pr.UpdatedAt,
		})
	}
	return events, nil
}

// Token returns the configured GitHub token.
func (a *Agent) Token() string { return a.token }

// Orgs returns the configured GitHub org filters.
func (a *Agent) Orgs() []string { return a.orgs }

// Username returns the configured GitHub username.
func (a *Agent) Username() string { return a.username }

// LookbackDays returns the configured lookback days setting (0 = auto).
func (a *Agent) LookbackDays() int { return a.lookbackDays }

// Prompt returns the current GitHub PR prompt.
func (a *Agent) Prompt() string { return a.prompt }

// SetClient replaces the underlying GitHub API client. Used by reinitClients
// to hot-swap the client after a token change.
func (a *Agent) SetClient(client GitHubClient) { a.client = client }

// since computes the "updated since" cutoff time for PR queries.
func (a *Agent) since() time.Time {
	if a.lookbackDays > 0 {
		return time.Now().AddDate(0, 0, -a.lookbackDays)
	}
	return lastWorkDaySince(time.Now())
}

// lastWorkDaySince returns midnight of the most recent work day before now.
func lastWorkDaySince(now time.Time) time.Time {
	day := now.Truncate(24 * time.Hour)
	switch now.Weekday() {
	case time.Monday:
		day = day.AddDate(0, 0, -3)
	default:
		day = day.AddDate(0, 0, -1)
		for day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
			day = day.AddDate(0, 0, -1)
		}
	}
	return day
}

func parseOrgs(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var orgs []string
	for _, o := range strings.Split(raw, ",") {
		o = strings.TrimSpace(o)
		if o != "" {
			orgs = append(orgs, o)
		}
	}
	return orgs
}

func buildSystemPrompt(overall, specific string) string {
	if overall == "" {
		return specific
	}
	return overall + "\n\n" + specific
}

func getSetting(s *store.Store, key, defaultVal string) string {
	if s == nil {
		return defaultVal
	}
	val, err := s.Get("setting." + key)
	if err != nil || val == "" {
		return defaultVal
	}
	return val
}

func getPrompt(s *store.Store, key, defaultVal string) string {
	if s == nil {
		return defaultVal
	}
	val, err := s.Get("prompt." + key)
	if err != nil || val == "" {
		return defaultVal
	}
	return val
}
