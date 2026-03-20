// Package agent defines the shared Agent interface and supporting types used
// by all officeagent service agents.
package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/darrint/officeagent/internal/store"
)

// Agent is the interface every service agent must satisfy.
//
// Check performs a health check for /doctor.
// Configure loads credentials and configuration from the kv store.
type Agent interface {
	Check(ctx context.Context) CheckResult
	Configure(store *store.Store) error
}

// CheckResult holds the outcome of a single health check.
type CheckResult struct {
	Name    string
	OK      bool
	Warn    bool // optional check; not a failure if unconfigured
	Detail  string
	Latency time.Duration
}

// Status returns a short human-readable status string.
func (c CheckResult) Status() string {
	if c.OK {
		return "OK"
	}
	if c.Warn {
		return "WARN"
	}
	return "FAIL"
}

// StatusClass returns the CSS class name corresponding to the check status.
func (c CheckResult) StatusClass() string {
	if c.OK {
		return "ok"
	}
	if c.Warn {
		return "warn"
	}
	return "fail"
}

// StatusIcon returns a single Unicode character representing the check status.
func (c CheckResult) StatusIcon() string {
	if c.OK {
		return "✓"
	}
	if c.Warn {
		return "⚠"
	}
	return "✗"
}

// LatencyStr returns a human-readable latency string.
func (c CheckResult) LatencyStr() string {
	if c.Latency < time.Millisecond {
		return fmt.Sprintf("%dµs", c.Latency.Microseconds())
	}
	return fmt.Sprintf("%dms", c.Latency.Milliseconds())
}
