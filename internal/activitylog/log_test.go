package activitylog_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/darrint/officeagent/internal/activitylog"
)

// TestLoggerWriteAndRead verifies that records written to a temp file can be
// read back as valid NDJSON.
func TestLoggerWriteAndRead(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "test.ndjson")
	l, err := activitylog.New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	l.Write(activitylog.Record{
		Direction: "req",
		Subsystem: "test",
		Method:    "GET",
		URL:       "http://example.com/foo",
	})
	l.Write(activitylog.Record{
		Direction:  "resp",
		Subsystem:  "test",
		Method:     "GET",
		URL:        "http://example.com/foo",
		StatusCode: 200,
		LatencyMS:  42,
		BodySnip:   "hello",
	})
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
	for i, line := range lines {
		var rec activitylog.Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Errorf("line %d: unmarshal: %v", i, err)
		}
	}
	// Spot-check second record.
	var rec activitylog.Record
	if err := json.Unmarshal([]byte(lines[1]), &rec); err != nil {
		t.Fatalf("unmarshal line 1: %v", err)
	}
	if rec.StatusCode != 200 {
		t.Errorf("StatusCode: got %d, want 200", rec.StatusCode)
	}
	if rec.BodySnip != "hello" {
		t.Errorf("BodySnip: got %q, want %q", rec.BodySnip, "hello")
	}
}

// TestNewDiscard verifies that NewDiscard does not panic and accepts writes.
func TestNewDiscard(t *testing.T) {
	t.Parallel()
	l := activitylog.NewDiscard()
	l.Write(activitylog.Record{Subsystem: "test"})
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestTransportRoundTrip verifies that Transport logs both req and resp records
// and passes the response body through to the caller unchanged.
func TestTransportRoundTrip(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, "teapot body")
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "transport.ndjson")
	l, err := activitylog.New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = l.Close() }()

	transport := &activitylog.Transport{Subsystem: "testsubsys", Log: l}
	client := &http.Client{Transport: transport}

	resp, err := client.Get(srv.URL + "/ping")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(body) != "teapot body" {
		t.Errorf("body: got %q, want %q", body, "teapot body")
	}
	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("status: got %d, want %d", resp.StatusCode, http.StatusTeapot)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	// Expect 2 lines: req + resp.
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d:\n%s", len(lines), string(data))
	}
	var req, respRec activitylog.Record
	if err := json.Unmarshal([]byte(lines[0]), &req); err != nil {
		t.Fatalf("unmarshal req: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &respRec); err != nil {
		t.Fatalf("unmarshal resp: %v", err)
	}
	if req.Direction != "req" {
		t.Errorf("req.Direction: got %q, want %q", req.Direction, "req")
	}
	if respRec.Direction != "resp" {
		t.Errorf("resp.Direction: got %q, want %q", respRec.Direction, "resp")
	}
	if respRec.StatusCode != http.StatusTeapot {
		t.Errorf("resp.StatusCode: got %d, want %d", respRec.StatusCode, http.StatusTeapot)
	}
	if !strings.Contains(respRec.BodySnip, "teapot body") {
		t.Errorf("resp.BodySnip: got %q, want to contain %q", respRec.BodySnip, "teapot body")
	}
	if req.Subsystem != "testsubsys" {
		t.Errorf("req.Subsystem: got %q, want %q", req.Subsystem, "testsubsys")
	}
}
