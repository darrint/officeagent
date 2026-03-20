// Package ntfy provides the ntfy service agent for officeagent.
// It owns the send-notification skill and health check.
package ntfy

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/darrint/officeagent/internal/agent"
	ntfyclient "github.com/darrint/officeagent/internal/ntfy"
	"github.com/darrint/officeagent/internal/store"
)

// Agent is the ntfy service agent.
type Agent struct {
	store *store.Store

	// config populated by Configure
	topic string
}

// New creates a new ntfy Agent.
// Call Configure to load settings from the store before using skills.
func New() *Agent {
	return &Agent{}
}

// Configure loads credentials and configuration from the kv store.
func (a *Agent) Configure(s *store.Store) error {
	a.store = s
	a.topic = strings.TrimSpace(getSetting(s, "ntfy_topic", ""))
	return nil
}

// Check performs a health check by sending an HTTP GET to the ntfy.sh base URL.
func (a *Agent) Check(ctx context.Context) agent.CheckResult {
	cr := agent.CheckResult{Name: "ntfy"}
	start := time.Now()

	if a.topic == "" {
		cr.Warn = true
		cr.Detail = "not configured — add ntfy topic via Settings (optional)"
		cr.Latency = time.Since(start)
		return cr
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://ntfy.sh", nil)
	if err != nil {
		cr.Detail = fmt.Sprintf("build request: %v", err)
		cr.Latency = time.Since(start)
		return cr
	}
	resp, err := http.DefaultClient.Do(req)
	cr.Latency = time.Since(start)
	if err != nil {
		cr.Detail = fmt.Sprintf("GET ntfy.sh failed: %v", err)
		return cr
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		cr.Detail = fmt.Sprintf("ntfy.sh returned status %d", resp.StatusCode)
		return cr
	}
	cr.OK = true
	cr.Detail = fmt.Sprintf("ntfy.sh reachable; topic=%q", a.topic)
	return cr
}

// Send posts a notification to the configured ntfy topic.
func (a *Agent) Send(ctx context.Context, title, body, clickURL string) error {
	if a.topic == "" {
		return fmt.Errorf("ntfy topic not configured")
	}
	return ntfyclient.Send(ctx, a.topic, title, body, clickURL)
}

// Topic returns the configured ntfy topic.
func (a *Agent) Topic() string { return a.topic }

// --- helpers ----------------------------------------------------------------

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
