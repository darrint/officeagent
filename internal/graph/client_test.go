package graph

// White-box tests for the graph package: pure helper functions and HTTP
// client methods tested against a local httptest server.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	_ "time/tzdata" // embed IANA timezone database for tests

	"golang.org/x/oauth2"
)

// fakeTokenProvider always returns a static, non-expired token.
type fakeTokenProvider struct{}

func (fakeTokenProvider) Token(_ context.Context) (*oauth2.Token, error) {
	return &oauth2.Token{
		AccessToken: "fake-access-token",
		Expiry:      time.Now().Add(time.Hour),
	}, nil
}

// newTestClient creates a graph.Client pointed at the given test server.
func newTestClient(ts *httptest.Server) *Client {
	c := &Client{
		auth:    fakeTokenProvider{},
		http:    ts.Client(),
		baseURL: ts.URL,
	}
	return c
}

// --- resolveLocation ---

func TestResolveLocation_empty(t *testing.T) {
	loc := resolveLocation("")
	if loc != time.UTC {
		t.Errorf("expected UTC for empty string, got %v", loc)
	}
}

func TestResolveLocation_UTC(t *testing.T) {
	loc := resolveLocation("UTC")
	if loc == nil {
		t.Fatal("expected non-nil location")
	}
}

func TestResolveLocation_IANA(t *testing.T) {
	loc := resolveLocation("America/New_York")
	if loc == nil || loc == time.UTC {
		t.Errorf("expected a real location for America/New_York, got %v", loc)
	}
}

func TestResolveLocation_windowsName(t *testing.T) {
	loc := resolveLocation("Eastern Standard Time")
	if loc == nil || loc == time.UTC {
		t.Errorf("expected non-UTC location for Eastern Standard Time, got %v", loc)
	}
}

func TestResolveLocation_unknown(t *testing.T) {
	loc := resolveLocation("Not/A/RealZone")
	if loc != time.UTC {
		t.Errorf("expected UTC fallback for unknown timezone, got %v", loc)
	}
}

// --- parseEventTime ---

func TestParseEventTime_UTC(t *testing.T) {
	et := parseEventTime("2024-06-15T14:30:00", "UTC")
	if et.IsZero() {
		t.Fatal("expected non-zero time")
	}
	if et.Hour() != 14 || et.Minute() != 30 {
		t.Errorf("expected 14:30, got %02d:%02d", et.Hour(), et.Minute())
	}
}

func TestParseEventTime_fractionalSeconds(t *testing.T) {
	et := parseEventTime("2024-06-15T14:30:00.0000000", "UTC")
	if et.IsZero() {
		t.Fatal("expected non-zero time")
	}
	if et.Hour() != 14 || et.Minute() != 30 {
		t.Errorf("expected 14:30, got %02d:%02d", et.Hour(), et.Minute())
	}
}

func TestParseEventTime_windowsTZ_offset(t *testing.T) {
	// Eastern Standard Time = UTC-5.
	// 09:00 Eastern = 14:00 UTC.
	et := parseEventTime("2024-01-15T09:00:00", "Eastern Standard Time")
	if et.IsZero() {
		t.Fatal("expected non-zero time")
	}
	utc := et.UTC()
	if utc.Hour() != 14 {
		t.Errorf("expected 14:00 UTC, got %02d:00 UTC", utc.Hour())
	}
}

func TestParseEventTime_invalidDate(t *testing.T) {
	et := parseEventTime("not-a-date", "UTC")
	if !et.IsZero() {
		t.Errorf("expected zero time for invalid date string, got %v", et)
	}
}

// --- ListMessages (mock HTTP) ---

func TestListMessages_success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/me/mailFolders/inbox/messages") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer fake-access-token" {
			t.Errorf("unexpected Authorization: %s", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"value": []map[string]any{
				{
					"id":                  "msg1",
					"subject":             "Test Subject",
					"receivedDateTime":    "2024-01-15T09:00:00Z",
					"bodyPreview":         "Hello there",
					"from": map[string]any{
						"emailAddress": map[string]any{
							"name":    "Alice",
							"address": "alice@example.com",
						},
					},
				},
			},
		})
	}))
	defer ts.Close()

	c := newTestClient(ts)
	msgs, err := c.ListMessages(context.Background(), 5)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	m := msgs[0]
	if m.ID != "msg1" {
		t.Errorf("ID: expected %q, got %q", "msg1", m.ID)
	}
	if m.Subject != "Test Subject" {
		t.Errorf("Subject: expected %q, got %q", "Test Subject", m.Subject)
	}
	if m.BodyPreview != "Hello there" {
		t.Errorf("BodyPreview: expected %q, got %q", "Hello there", m.BodyPreview)
	}
	if m.From != "Alice <alice@example.com>" {
		t.Errorf("From: expected %q, got %q", "Alice <alice@example.com>", m.From)
	}
}

func TestListMessages_empty(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"value": []any{}})
	}))
	defer ts.Close()

	c := newTestClient(ts)
	msgs, err := c.ListMessages(context.Background(), 5)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("expected 0 messages, got %d", len(msgs))
	}
}

func TestListMessages_httpError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"code": "Unauthorized", "message": "token expired"},
		})
	}))
	defer ts.Close()

	c := newTestClient(ts)
	_, err := c.ListMessages(context.Background(), 5)
	if err == nil {
		t.Fatal("expected error for non-200 response")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("expected 401 in error, got: %v", err)
	}
}

// --- ListEvents (mock HTTP) ---

func TestListEvents_success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/me/calendarview") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		prefer := r.Header.Get("Prefer")
		if !strings.Contains(prefer, "outlook.timezone") {
			t.Errorf("expected Prefer header with outlook.timezone, got %q", prefer)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"value": []map[string]any{
				{
					"id":      "ev1",
					"subject": "Standup",
					"start":   map[string]any{"dateTime": "2024-06-15T10:00:00", "timeZone": "UTC"},
					"end":     map[string]any{"dateTime": "2024-06-15T10:30:00", "timeZone": "UTC"},
				},
			},
		})
	}))
	defer ts.Close()

	c := newTestClient(ts)
	events, err := c.ListEvents(context.Background(), 5)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	e := events[0]
	if e.ID != "ev1" {
		t.Errorf("ID: expected %q, got %q", "ev1", e.ID)
	}
	if e.Subject != "Standup" {
		t.Errorf("Subject: expected %q, got %q", "Standup", e.Subject)
	}
	if e.Start.Hour() != 10 || e.Start.Minute() != 0 {
		t.Errorf("Start: expected 10:00, got %02d:%02d", e.Start.Hour(), e.Start.Minute())
	}
	if e.End.Hour() != 10 || e.End.Minute() != 30 {
		t.Errorf("End: expected 10:30, got %02d:%02d", e.End.Hour(), e.End.Minute())
	}
}

func TestListEvents_httpError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"code": "Forbidden", "message": "access denied"},
		})
	}))
	defer ts.Close()

	c := newTestClient(ts)
	_, err := c.ListEvents(context.Background(), 5)
	if err == nil {
		t.Fatal("expected error for non-200 response")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("expected 403 in error, got: %v", err)
	}
}

// --- GetMe (mock HTTP) ---

func TestGetMe_success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/me") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"displayName":       "John Doe",
			"mail":              "john@example.com",
			"userPrincipalName": "john@example.com",
		})
	}))
	defer ts.Close()

	c := newTestClient(ts)
	u, err := c.GetMe(context.Background())
	if err != nil {
		t.Fatalf("GetMe: %v", err)
	}
	if u.DisplayName != "John Doe" {
		t.Errorf("DisplayName: expected %q, got %q", "John Doe", u.DisplayName)
	}
	if u.Mail != "john@example.com" {
		t.Errorf("Mail: expected %q, got %q", "john@example.com", u.Mail)
	}
}

func TestGetMe_httpError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"code": "Unauthorized", "message": "not authenticated"},
		})
	}))
	defer ts.Close()

	c := newTestClient(ts)
	_, err := c.GetMe(context.Background())
	if err == nil {
		t.Fatal("expected error for non-200 response")
	}
}
