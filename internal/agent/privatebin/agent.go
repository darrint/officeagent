// Package privatebin provides the PrivateBin service agent for officeagent.
// It owns the post-briefing-paste skill and health check.
package privatebin

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/darrint/officeagent/internal/agent"
	pbclient "github.com/darrint/officeagent/internal/privatebin"
	"github.com/darrint/officeagent/internal/store"
)

const defaultInstanceURL = "https://privatebin.net"

// Agent is the PrivateBin service agent.
type Agent struct {
	store *store.Store

	// config populated by Configure
	instanceURL string
}

// New creates a new PrivateBin Agent.
// Call Configure to load settings from the store before using skills.
func New() *Agent {
	return &Agent{instanceURL: defaultInstanceURL}
}

// Configure loads credentials and configuration from the kv store.
func (a *Agent) Configure(s *store.Store) error {
	a.store = s
	a.instanceURL = strings.TrimSpace(getSetting(s, "privatebin_url", defaultInstanceURL))
	if a.instanceURL == "" {
		a.instanceURL = defaultInstanceURL
	}
	return nil
}

// Check performs a health check by sending an HTTP GET to the configured
// PrivateBin instance and verifying it returns a 200 response.
func (a *Agent) Check(ctx context.Context) agent.CheckResult {
	cr := agent.CheckResult{Name: "PrivateBin"}
	start := time.Now()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.instanceURL, nil)
	if err != nil {
		cr.Detail = fmt.Sprintf("build request: %v", err)
		cr.Latency = time.Since(start)
		return cr
	}
	resp, err := http.DefaultClient.Do(req)
	cr.Latency = time.Since(start)
	if err != nil {
		cr.Detail = fmt.Sprintf("GET %s failed: %v", a.instanceURL, err)
		return cr
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		cr.Detail = fmt.Sprintf("%s returned status %d", a.instanceURL, resp.StatusCode)
		return cr
	}
	cr.OK = true
	cr.Detail = fmt.Sprintf("reachable (%s)", a.instanceURL)
	return cr
}

// PostBriefing encrypts the markdown content and posts it to the configured
// PrivateBin instance. Returns the shareable paste URL (with key in fragment).
func (a *Agent) PostBriefing(ctx context.Context, markdown []byte) (string, error) {
	return pbclient.PostPaste(ctx, a.instanceURL, markdown)
}

// InstanceURL returns the configured PrivateBin instance URL.
func (a *Agent) InstanceURL() string { return a.instanceURL }

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
