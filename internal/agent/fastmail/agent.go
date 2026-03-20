// Package fastmail provides the Fastmail service agent for officeagent.
// It owns personal email summary and low-priority archive skills.
package fastmail

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/darrint/officeagent/internal/agent"
	fastmailclient "github.com/darrint/officeagent/internal/fastmail"
	"github.com/darrint/officeagent/internal/llm"
	"github.com/darrint/officeagent/internal/store"
)

// DefaultFastmailPrompt is the default system prompt for Fastmail summaries.
const DefaultFastmailPrompt = "You are a helpful personal assistant. Give the user a concise summary of their recent personal inbox. Highlight anything that needs attention or action. Be friendly but brief."

// LLMClient is the subset of llm.Client used by the agent's skills.
type LLMClient interface {
	Chat(ctx context.Context, messages []llm.Message) (string, error)
}

// fastmailMover extends the base client with archive capabilities.
type fastmailMover interface {
	ListMessages(ctx context.Context, top int) ([]fastmailclient.Message, error)
	GetOrCreateMailbox(ctx context.Context, name string) (string, error)
	MoveMessages(ctx context.Context, messageIDs []string, targetMailboxID string) error
}

// readOnlyChecker is optionally implemented by the Fastmail client.
type readOnlyChecker interface {
	IsReadOnly(ctx context.Context) (bool, error)
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

// Agent is the Fastmail service agent.
type Agent struct {
	client *fastmailclient.Client
	store  *store.Store

	// config populated by Configure
	token         string
	lowPrioFolder string
	prompt        string
	overallPrompt string
}

// New creates a new Fastmail Agent.
// client may be nil if no token has been configured yet.
// Call Configure to load settings from the store before using skills.
func New(client *fastmailclient.Client) *Agent {
	return &Agent{
		client:        client,
		lowPrioFolder: "Low Priority",
		prompt:        DefaultFastmailPrompt,
	}
}

// Configure loads credentials and configuration from the kv store.
func (a *Agent) Configure(s *store.Store) error {
	a.store = s
	a.token = getSetting(s, "fastmail_token", "")
	a.lowPrioFolder = getSetting(s, "fastmail_lowprio_folder", "Low Priority")
	a.prompt = getPrompt(s, "fastmail", DefaultFastmailPrompt)
	a.overallPrompt = getPrompt(s, "overall", "")
	return nil
}

// Check performs a health check against the Fastmail JMAP API.
func (a *Agent) Check(ctx context.Context) agent.CheckResult {
	cr := agent.CheckResult{Name: "Fastmail (JMAP)"}
	start := time.Now()
	if a.client == nil {
		cr.Warn = true
		cr.Detail = "not configured — add Fastmail token via Settings (optional)"
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
	// Warn if the token is read-only (archive will fail).
	if checker, ok := any(a.client).(readOnlyChecker); ok {
		if readOnly, roErr := checker.IsReadOnly(ctx); roErr == nil && readOnly {
			cr.OK = false
			cr.Warn = true
			cr.Detail += " — token is read-only; mail moving will fail. Regenerate the Fastmail token with full (read+write) access."
		}
	}
	return cr
}

// EmailSummary fetches the user's recent personal email and returns an
// LLM-generated markdown summary together with any low-priority message
// candidates.
func (a *Agent) EmailSummary(ctx context.Context, llmC LLMClient, feedbackCtx string) (summary string, lowPrios []LowPrioMsg, err error) {
	if a.client == nil {
		return "", nil, fmt.Errorf("fastmail not configured")
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

	sysPrompt := buildSystemPrompt(a.overallPrompt, a.prompt+feedbackCtx)
	reply, err := llmC.Chat(ctx, []llm.Message{
		{Role: "system", Content: sysPrompt},
		{Role: "user", Content: "Here are my recent personal emails:\n\n" + sb.String()},
	})
	if err != nil {
		return "", nil, fmt.Errorf("LLM chat: %w", err)
	}

	// Classify low-priority messages using the already-fetched list.
	proposed, classErr := classifyLowPriority(ctx, classifyMsgs, llmC)
	if classErr != nil {
		log.Printf("fastmail agent: classify low-prio: %v", classErr)
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

// ArchiveLowPrio moves low-priority messages to the configured mailbox.
// cachedIDs is the list of message IDs identified as low-priority in the most
// recent briefing run; pass nil to trigger a live LLM re-classification.
// Returns the number of messages moved.
func (a *Agent) ArchiveLowPrio(ctx context.Context, llmC LLMClient, cachedIDs []string) (int, error) {
	if a.client == nil {
		return 0, nil // not configured — skip silently
	}
	mover, ok := any(a.client).(fastmailMover)
	if !ok {
		return 0, nil // client doesn't support moving
	}

	ids := cachedIDs
	if ids == nil {
		rawMsgs, listErr := mover.ListMessages(ctx, 30)
		if listErr != nil {
			return 0, fmt.Errorf("list messages: %w", listErr)
		}
		msgs := make([]classifyMsg, len(rawMsgs))
		for i, m := range rawMsgs {
			msgs[i] = classifyMsg{ID: m.ID, From: m.From, Subject: m.Subject, Preview: m.BodyPreview}
		}
		proposed, classErr := classifyLowPriority(ctx, msgs, llmC)
		if classErr != nil {
			return 0, classErr
		}
		ids = filterToKnownIDs(proposed, msgs)
	}

	if len(ids) == 0 {
		return 0, nil
	}

	mailboxID, err := mover.GetOrCreateMailbox(ctx, a.lowPrioFolder)
	if err != nil {
		return 0, fmt.Errorf("get/create mailbox: %w", err)
	}
	if err := mover.MoveMessages(ctx, ids, mailboxID); err != nil {
		return 0, fmt.Errorf("move messages: %w", err)
	}
	return len(ids), nil
}

// SetClient replaces the underlying Fastmail API client. Used by reinitClients
// when a new token is saved via the Settings page.
func (a *Agent) SetClient(client *fastmailclient.Client) { a.client = client }

// Token returns the configured Fastmail API token.
func (a *Agent) Token() string { return a.token }

// LowPrioFolder returns the configured low-priority mailbox name.
func (a *Agent) LowPrioFolder() string { return a.lowPrioFolder }

// Prompt returns the current Fastmail prompt.
func (a *Agent) Prompt() string { return a.prompt }

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
