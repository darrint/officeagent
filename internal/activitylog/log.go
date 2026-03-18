// Package activitylog writes structured NDJSON activity records to a file that
// is separate from the main SQLite config store. Each line is a JSON object
// that can be queried with DuckDB, jq, or any tool that understands NDJSON.
//
// The log file is opened in append mode so records accumulate across restarts.
// Log writes are best-effort: errors are printed to stderr but never propagate
// to callers. The file is NOT rotated; operators can truncate or archive it
// manually.
package activitylog

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

// Record is a single activity log entry.
type Record struct {
	Timestamp  time.Time `json:"ts"`
	Direction  string    `json:"dir"`        // "req" or "resp"
	Subsystem  string    `json:"subsystem"`  // "llm", "graph", "fastmail", "github", "ntfy", "ui"
	Method     string    `json:"method"`
	URL        string    `json:"url"`
	StatusCode int       `json:"status_code,omitempty"`
	LatencyMS  int64     `json:"latency_ms,omitempty"`
	Error      string    `json:"error,omitempty"`
	BodySnip   string    `json:"body_snip,omitempty"` // first 512 bytes of response body
}

const maxBodySnip = 512

// Logger appends NDJSON records to a file.
type Logger struct {
	mu sync.Mutex
	w  io.WriteCloser
}

// New opens (or creates) the NDJSON log file at path in append mode.
// The caller is responsible for calling Close.
func New(path string) (*Logger, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &Logger{w: f}, nil
}

// discardWriteCloser is an io.WriteCloser that discards all writes.
type discardWriteCloser struct{}

func (discardWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (discardWriteCloser) Close() error                { return nil }

// NewDiscard returns a Logger that drops all records. Useful in tests or when
// no log path is configured.
func NewDiscard() *Logger {
	return &Logger{w: discardWriteCloser{}}
}

// Write appends a record. Errors are logged to stderr; they do not propagate.
func (l *Logger) Write(r Record) {
	b, err := json.Marshal(r)
	if err != nil {
		log.Printf("activitylog: marshal: %v", err)
		return
	}
	b = append(b, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.w.Write(b); err != nil {
		log.Printf("activitylog: write: %v", err)
	}
}

// Close flushes and closes the underlying file.
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Close()
}

// Transport is an http.RoundTripper that logs each request/response pair.
type Transport struct {
	Subsystem string
	Next      http.RoundTripper // wrapped transport; defaults to http.DefaultTransport
	Log       *Logger
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	next := t.Next
	if next == nil {
		next = http.DefaultTransport
	}

	t.Log.Write(Record{
		Timestamp: time.Now().UTC(),
		Direction: "req",
		Subsystem: t.Subsystem,
		Method:    req.Method,
		URL:       req.URL.String(),
	})

	start := time.Now()
	resp, err := next.RoundTrip(req)
	latency := time.Since(start).Milliseconds()

	if err != nil {
		t.Log.Write(Record{
			Timestamp: time.Now().UTC(),
			Direction: "resp",
			Subsystem: t.Subsystem,
			Method:    req.Method,
			URL:       req.URL.String(),
			LatencyMS: latency,
			Error:     err.Error(),
		})
		return nil, err
	}

	// Read a snippet of the response body without consuming it.
	var snip string
	if resp.Body != nil {
		var buf bytes.Buffer
		if _, rerr := io.Copy(&buf, io.LimitReader(resp.Body, maxBodySnip)); rerr == nil {
			snip = buf.String()
			// Restore the body so callers can still read it.
			resp.Body = io.NopCloser(io.MultiReader(&buf, resp.Body))
		}
	}

	t.Log.Write(Record{
		Timestamp:  time.Now().UTC(),
		Direction:  "resp",
		Subsystem:  t.Subsystem,
		Method:     req.Method,
		URL:        req.URL.String(),
		StatusCode: resp.StatusCode,
		LatencyMS:  latency,
		BodySnip:   snip,
	})

	return resp, nil
}
