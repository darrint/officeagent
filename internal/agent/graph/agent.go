// Package graph provides the Microsoft Graph service agent for officeagent.
// It owns email summary, calendar summary, and low-priority archive skills.
package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/darrint/officeagent/internal/agent"
	graphclient "github.com/darrint/officeagent/internal/graph"
	"github.com/darrint/officeagent/internal/llm"
	"github.com/darrint/officeagent/internal/store"
)

// Default system prompts.
const (
	DefaultEmailPrompt    = "You are a helpful executive assistant. Give the user a concise summary of their recent inbox. Highlight anything urgent or requiring action. Be friendly but brief."
	DefaultCalendarPrompt = "You are a helpful executive assistant. Give the user a concise morning briefing of their upcoming calendar events. Be friendly but brief."
)

// LLMClient is the subset of llm.Client used by the agent's skills.
type LLMClient interface {
	Chat(ctx context.Context, messages []llm.Message) (string, error)
}

// GraphClient is the interface the agent requires from the Graph API client.
// It covers basic read operations plus optional archive capabilities.
// GetOrCreateFolder and MoveMessages are used only by ArchiveLowPrio; if the
// underlying client does not implement them ArchiveLowPrio returns (0,0,nil).
type GraphClient interface {
	ListMessages(ctx context.Context, top int) ([]graphclient.Message, error)
	ListEvents(ctx context.Context, top int) ([]graphclient.Event, error)
	GetOrCreateFolder(ctx context.Context, name string) (string, error)
	MoveMessages(ctx context.Context, messageIDs []string, folderID string) (moved, skipped int, err error)
}

// classifyMsg is a compact message descriptor for LLM classification.
type classifyMsg struct {
	ID      string
	From    string
	Subject string
	Preview string
}

// LowPrioMsg is a message identified as low-priority during a briefing run.
// Exported so the server can embed it in its cachedReport type.
type LowPrioMsg struct {
	ID         string    `json:"id"`
	From       string    `json:"from"`
	Subject    string    `json:"subject"`
	ReceivedAt time.Time `json:"received_at"`
}

// Agent is the Microsoft Graph service agent.
type Agent struct {
	auth   *graphclient.Auth
	client GraphClient
	store  *store.Store

	// config populated by Configure
	lowPrioFolder  string
	emailPrompt    string
	calendarPrompt string
	overallPrompt  string
}

// New creates a new Graph Agent with the given auth and client.
// Call Configure to load settings from the store before using skills.
func New(auth *graphclient.Auth, client GraphClient) *Agent {
	return &Agent{
		auth:           auth,
		client:         client,
		lowPrioFolder:  "Low Priority",
		emailPrompt:    DefaultEmailPrompt,
		calendarPrompt: DefaultCalendarPrompt,
	}
}

// Configure loads credentials and configuration from the kv store.
func (a *Agent) Configure(s *store.Store) error {
	a.store = s
	a.lowPrioFolder = getSetting(s, "graph_lowprio_folder", "Low Priority")
	a.emailPrompt = getPrompt(s, "email", DefaultEmailPrompt)
	a.calendarPrompt = getPrompt(s, "calendar", DefaultCalendarPrompt)
	a.overallPrompt = getPrompt(s, "overall", "")
	return nil
}

// Check performs a health check against Microsoft Graph (mail and calendar).
func (a *Agent) Check(ctx context.Context) agent.CheckResult {
	cr := agent.CheckResult{Name: "Graph (mail)"}
	start := time.Now()
	if !a.auth.IsAuthenticated(ctx) {
		cr.Detail = "not authenticated — visit /login"
		cr.Latency = time.Since(start)
		return cr
	}
	msgs, err := a.client.ListMessages(ctx, 1)
	cr.Latency = time.Since(start)
	if err != nil {
		cr.Detail = fmt.Sprintf("ListMessages failed: %v", err)
		return cr
	}
	cr.OK = true
	cr.Detail = fmt.Sprintf("OK (%d message(s) accessible)", len(msgs))
	return cr
}

// CheckCalendar performs a health check against Microsoft Graph (calendar).
func (a *Agent) CheckCalendar(ctx context.Context) agent.CheckResult {
	cr := agent.CheckResult{Name: "Graph (calendar)"}
	start := time.Now()
	if !a.auth.IsAuthenticated(ctx) {
		cr.Detail = "not authenticated — visit /login"
		cr.Latency = time.Since(start)
		return cr
	}
	events, err := a.client.ListEvents(ctx, 1)
	cr.Latency = time.Since(start)
	if err != nil {
		cr.Detail = fmt.Sprintf("ListEvents failed: %v", err)
		return cr
	}
	cr.OK = true
	cr.Detail = fmt.Sprintf("OK (%d event(s) accessible)", len(events))
	return cr
}

// EmailSummary fetches the user's recent work email and returns an LLM-generated
// markdown summary together with any low-priority message candidates.
// The raw markdown string and the low-priority list are returned so the server
// can cache them without depending on this package's internal types.
func (a *Agent) EmailSummary(ctx context.Context, llmC LLMClient, feedbackCtx string) (summary string, lowPrios []LowPrioMsg, err error) {
	if a.client == nil {
		return "", nil, fmt.Errorf("graph not configured")
	}
	msgs, err := a.client.ListMessages(ctx, 20)
	if err != nil {
		return "", nil, fmt.Errorf("list messages: %w", err)
	}

	classifyMsgs := make([]classifyMsg, len(msgs))
	var sb strings.Builder
	if len(msgs) == 0 {
		sb.WriteString("No recent messages.")
	} else {
		for i, m := range msgs {
			classifyMsgs[i] = classifyMsg{ID: m.ID, From: m.From, Subject: m.Subject, Preview: m.BodyPreview}
			fmt.Fprintf(&sb, "- From: %s | Subject: %s | Received: %s\n  Preview: %s\n",
				m.From, m.Subject,
				m.ReceivedAt.UTC().Format("Mon Jan 2 3:04 PM UTC"),
				m.BodyPreview,
			)
		}
	}

	sysPrompt := buildSystemPrompt(a.overallPrompt, a.emailPrompt+feedbackCtx)
	reply, err := llmC.Chat(ctx, []llm.Message{
		{Role: "system", Content: sysPrompt},
		{Role: "user", Content: "Here are my recent emails:\n\n" + sb.String()},
	})
	if err != nil {
		return "", nil, fmt.Errorf("LLM chat: %w", err)
	}

	// Classify low-priority messages using the already-fetched list.
	proposed, classErr := classifyLowPriority(ctx, classifyMsgs, llmC)
	if classErr != nil {
		log.Printf("graph agent: classify low-prio: %v", classErr)
	} else {
		ids := filterToKnownIDs(proposed, classifyMsgs)
		idSet := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			idSet[id] = struct{}{}
		}
		for _, m := range msgs {
			if _, ok := idSet[m.ID]; ok {
				lowPrios = append(lowPrios, LowPrioMsg{
					ID:         m.ID,
					From:       m.From,
					Subject:    m.Subject,
					ReceivedAt: m.ReceivedAt,
				})
			}
		}
	}

	return reply, lowPrios, nil
}

// CalendarSummary fetches the user's upcoming calendar events and returns an
// LLM-generated markdown summary.
func (a *Agent) CalendarSummary(ctx context.Context, llmC LLMClient, feedbackCtx string) (string, error) {
	if a.client == nil {
		return "", fmt.Errorf("graph not configured")
	}
	events, err := a.client.ListEvents(ctx, 20)
	if err != nil {
		return "", fmt.Errorf("list events: %w", err)
	}

	var sb strings.Builder
	if len(events) == 0 {
		sb.WriteString("No upcoming events.")
	} else {
		for _, e := range events {
			fmt.Fprintf(&sb, "- %s: %s to %s\n",
				e.Subject,
				e.Start.UTC().Format("Mon Jan 2 3:04 PM UTC"),
				e.End.UTC().Format("3:04 PM UTC"),
			)
		}
	}

	sysPrompt := buildSystemPrompt(a.overallPrompt, a.calendarPrompt+feedbackCtx)
	reply, err := llmC.Chat(ctx, []llm.Message{
		{Role: "system", Content: sysPrompt},
		{Role: "user", Content: "Here are my upcoming calendar events:\n\n" + sb.String()},
	})
	if err != nil {
		return "", fmt.Errorf("LLM chat: %w", err)
	}
	return reply, nil
}

// ArchiveLowPrio moves low-priority messages to the configured folder.
// cachedIDs is the list of message IDs identified as low-priority in the most
// recent briefing run; pass nil to trigger a live LLM re-classification.
// Returns the number of messages moved and skipped.
func (a *Agent) ArchiveLowPrio(ctx context.Context, llmC LLMClient, cachedIDs []string) (moved, skipped int, err error) {
	if a.client == nil {
		return 0, 0, nil
	}

	ids := cachedIDs
	if ids == nil {
		// No cached IDs — re-classify live.
		rawMsgs, listErr := a.client.ListMessages(ctx, 30)
		if listErr != nil {
			return 0, 0, fmt.Errorf("list messages: %w", listErr)
		}
		msgs := make([]classifyMsg, len(rawMsgs))
		for i, m := range rawMsgs {
			msgs[i] = classifyMsg{ID: m.ID, From: m.From, Subject: m.Subject, Preview: m.BodyPreview}
		}
		proposed, classErr := classifyLowPriority(ctx, msgs, llmC)
		if classErr != nil {
			return 0, 0, classErr
		}
		ids = filterToKnownIDs(proposed, msgs)
	}

	if len(ids) == 0 {
		return 0, 0, nil
	}

	folderID, err := a.client.GetOrCreateFolder(ctx, a.lowPrioFolder)
	if err != nil {
		return 0, 0, fmt.Errorf("get/create folder: %w", err)
	}
	moved, skipped, err = a.client.MoveMessages(ctx, ids, folderID)
	if err != nil {
		return moved, skipped, fmt.Errorf("move messages: %w", err)
	}
	return moved, skipped, nil
}

// LowPrioFolder returns the configured low-priority folder name.
func (a *Agent) LowPrioFolder() string { return a.lowPrioFolder }

// EmailPrompt returns the current email prompt.
func (a *Agent) EmailPrompt() string { return a.emailPrompt }

// CalendarPrompt returns the current calendar prompt.
func (a *Agent) CalendarPrompt() string { return a.calendarPrompt }

// OverallPrompt returns the current overall prompt.
func (a *Agent) OverallPrompt() string { return a.overallPrompt }

// IsAuthenticated reports whether the Graph OAuth token is present and valid.
func (a *Agent) IsAuthenticated(ctx context.Context) bool {
	return a.auth.IsAuthenticated(ctx)
}

// --- helpers ----------------------------------------------------------------

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

func classifyLowPriority(ctx context.Context, msgs []classifyMsg, llmC LLMClient) ([]string, error) {
	if len(msgs) == 0 {
		return nil, nil
	}
	var sb strings.Builder
	for _, m := range msgs {
		fmt.Fprintf(&sb, "ID: %s\nFrom: %s\nSubject: %s\nPreview: %s\n\n", m.ID, m.From, m.Subject, m.Preview)
	}
	systemPrompt := "You are an email assistant. Identify which of the following emails are low priority: newsletters, marketing, automated notifications, promotional offers, social media digests, and other non-actionable bulk messages. Return ONLY a JSON array of IDs for the low-priority messages. No explanation, no markdown, just the JSON array. If none are low priority return []."
	reply, err := llmC.Chat(ctx, []llm.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: sb.String()},
	})
	if err != nil {
		return nil, fmt.Errorf("LLM classify: %w", err)
	}
	return parseLLMIDs(reply), nil
}

func filterToKnownIDs(proposed []string, known []classifyMsg) []string {
	set := make(map[string]struct{}, len(known))
	for _, m := range known {
		set[m.ID] = struct{}{}
	}
	var out []string
	for _, id := range proposed {
		if _, ok := set[id]; ok {
			out = append(out, id)
		}
	}
	return out
}

func parseLLMIDs(reply string) []string {
	s := strings.TrimSpace(reply)
	if idx := strings.Index(s, "```"); idx >= 0 {
		s = s[idx+3:]
		if nl := strings.Index(s, "\n"); nl >= 0 {
			s = s[nl+1:]
		}
		if end := strings.LastIndex(s, "```"); end >= 0 {
			s = s[:end]
		}
	}
	s = strings.TrimSpace(s)
	start := strings.Index(s, "[")
	end := strings.LastIndex(s, "]")
	if start < 0 || end <= start {
		return nil
	}
	var ids []string
	if err := json.Unmarshal([]byte(s[start:end+1]), &ids); err != nil {
		log.Printf("parseLLMIDs: unmarshal error: %v (input: %q)", err, s[start:end+1])
		return nil
	}
	return ids
}
