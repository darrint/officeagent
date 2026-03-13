package server

// White-box tests for server-package pure functions and HTTP handlers.
// Uses fake interface implementations to avoid requiring real OAuth, Graph, or LLM calls.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/darrint/officeagent/internal/config"
	"github.com/darrint/officeagent/internal/graph"
	"github.com/darrint/officeagent/internal/llm"
	"github.com/darrint/officeagent/internal/store"
	"golang.org/x/oauth2"
)

// --- fakes ---

// fakeAuth implements authService. Authenticated controls IsAuthenticated.
type fakeAuth struct {
	authenticated bool
}

func (f fakeAuth) IsAuthenticated(_ context.Context) bool { return f.authenticated }
func (f fakeAuth) AuthCodeURL(_ string) (string, string, string, error) {
	return "https://login.example.com/auth", "state", "verifier", nil
}
func (f fakeAuth) ExchangeCode(_ context.Context, _, _, _ string) (*oauth2.Token, error) {
	return &oauth2.Token{AccessToken: "tok"}, nil
}

// fakeGraph implements graphService.
type fakeGraph struct {
	msgs    []graph.Message
	events  []graph.Event
	msgsErr error
	evtsErr error
}

func (f fakeGraph) ListMessages(_ context.Context, _ int) ([]graph.Message, error) {
	return f.msgs, f.msgsErr
}
func (f fakeGraph) ListEvents(_ context.Context, _ int) ([]graph.Event, error) {
	return f.events, f.evtsErr
}

// fakeLLM implements llmService.
type fakeLLM struct {
	reply string
	err   error
}

func (f fakeLLM) Chat(_ context.Context, _ []llm.Message) (string, error) {
	return f.reply, f.err
}

// --- helpers ---

// newTestServer builds a minimal Server for handler tests.
func newTestServer(t *testing.T, auth authService, st *store.Store) *Server {
	t.Helper()
	cfg := config.Default()
	s := &Server{
		cfg:           cfg,
		mux:           http.NewServeMux(),
		auth:          auth,
		client:        fakeGraph{},
		store:         st,
		pendingLogins: make(map[string]pendingLogin),
	}
	s.routes()
	return s
}

func newMemStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// --- renderMarkdown ---

func TestRenderMarkdown_basic(t *testing.T) {
	html := renderMarkdown("**bold** and _italic_")
	s := string(html)
	if !strings.Contains(s, "<strong>bold</strong>") {
		t.Errorf("expected <strong>bold</strong> in output, got: %s", s)
	}
	if !strings.Contains(s, "<em>italic</em>") {
		t.Errorf("expected <em>italic</em> in output, got: %s", s)
	}
}

func TestRenderMarkdown_heading(t *testing.T) {
	html := renderMarkdown("# Title")
	s := string(html)
	if !strings.Contains(s, "<h1>") {
		t.Errorf("expected <h1> in output, got: %s", s)
	}
}

func TestRenderMarkdown_list(t *testing.T) {
	html := renderMarkdown("- item one\n- item two")
	s := string(html)
	if !strings.Contains(s, "<li>") {
		t.Errorf("expected <li> in output, got: %s", s)
	}
}

// --- getPrompt ---

func TestGetPrompt_defaultWhenNoStore(t *testing.T) {
	s := &Server{}
	got := s.getPrompt("email", "default prompt")
	if got != "default prompt" {
		t.Errorf("expected default prompt, got %q", got)
	}
}

func TestGetPrompt_defaultWhenKeyMissing(t *testing.T) {
	st := newMemStore(t)
	s := &Server{store: st}
	got := s.getPrompt("email", "default prompt")
	if got != "default prompt" {
		t.Errorf("expected default prompt when key absent, got %q", got)
	}
}

func TestGetPrompt_fromStore(t *testing.T) {
	st := newMemStore(t)
	if err := st.Set("prompt.email", "custom prompt"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	s := &Server{store: st}
	got := s.getPrompt("email", "default prompt")
	if got != "custom prompt" {
		t.Errorf("expected %q, got %q", "custom prompt", got)
	}
}

// --- feedbackContext ---

func TestFeedbackContext_noStore(t *testing.T) {
	s := &Server{}
	got := s.feedbackContext("email")
	if got != "" {
		t.Errorf("expected empty string with no store, got %q", got)
	}
}

func TestFeedbackContext_empty(t *testing.T) {
	st := newMemStore(t)
	s := &Server{store: st}
	got := s.feedbackContext("email")
	if got != "" {
		t.Errorf("expected empty string with no feedback, got %q", got)
	}
}

func TestFeedbackContext_withFeedback(t *testing.T) {
	st := newMemStore(t)
	if err := st.AddFeedback("email", "bad", "too verbose"); err != nil {
		t.Fatalf("AddFeedback: %v", err)
	}
	if err := st.AddFeedback("email", "good", "perfect length"); err != nil {
		t.Fatalf("AddFeedback: %v", err)
	}
	s := &Server{store: st}
	got := s.feedbackContext("email")
	if !strings.Contains(got, "feedback") {
		t.Errorf("expected 'feedback' in output, got: %q", got)
	}
	if !strings.Contains(got, "too verbose") {
		t.Errorf("expected note 'too verbose' in output, got: %q", got)
	}
	if !strings.Contains(got, "perfect length") {
		t.Errorf("expected note 'perfect length' in output, got: %q", got)
	}
}

func TestFeedbackContext_sectionIsolated(t *testing.T) {
	st := newMemStore(t)
	if err := st.AddFeedback("calendar", "bad", "calendar note"); err != nil {
		t.Fatalf("AddFeedback: %v", err)
	}
	s := &Server{store: st}
	// email context should not include calendar feedback
	got := s.feedbackContext("email")
	if strings.Contains(got, "calendar note") {
		t.Errorf("email feedback context should not contain calendar notes, got: %q", got)
	}
}

// --- handleHealth ---

func TestHandleHealth(t *testing.T) {
	srv := newTestServer(t, fakeAuth{authenticated: true}, nil)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"ok"`) {
		t.Errorf("expected 'ok' in response body, got: %s", body)
	}
}

// --- handleFeedback validation ---

func TestHandleFeedback_invalidSection(t *testing.T) {
	srv := newTestServer(t, fakeAuth{authenticated: true}, newMemStore(t))
	form := url.Values{"section": {"invalid"}, "rating": {"good"}, "note": {""}}
	req := httptest.NewRequest(http.MethodPost, "/feedback", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid section, got %d", w.Code)
	}
}

func TestHandleFeedback_invalidRating(t *testing.T) {
	srv := newTestServer(t, fakeAuth{authenticated: true}, newMemStore(t))
	form := url.Values{"section": {"email"}, "rating": {"meh"}, "note": {""}}
	req := httptest.NewRequest(http.MethodPost, "/feedback", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid rating, got %d", w.Code)
	}
}

func TestHandleFeedback_unauthenticated(t *testing.T) {
	srv := newTestServer(t, fakeAuth{authenticated: false}, newMemStore(t))
	form := url.Values{"section": {"email"}, "rating": {"good"}, "note": {""}}
	req := httptest.NewRequest(http.MethodPost, "/feedback", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for unauthenticated feedback, got %d", w.Code)
	}
}

func TestHandleFeedback_valid(t *testing.T) {
	st := newMemStore(t)
	srv := newTestServer(t, fakeAuth{authenticated: true}, st)
	form := url.Values{"section": {"email"}, "rating": {"good"}, "note": {"nice summary"}}
	req := httptest.NewRequest(http.MethodPost, "/feedback", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	// valid feedback redirects back to /
	if w.Code != http.StatusSeeOther {
		t.Errorf("expected 303 redirect, got %d", w.Code)
	}
	// confirm it was stored
	entries, err := st.RecentFeedback("email", 1)
	if err != nil {
		t.Fatalf("RecentFeedback: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 feedback entry stored, got %d", len(entries))
	}
	if entries[0].Note != "nice summary" {
		t.Errorf("expected note %q, got %q", "nice summary", entries[0].Note)
	}
}

// --- handleLoginStatus ---

func TestHandleLoginStatus_authenticated(t *testing.T) {
	srv := newTestServer(t, fakeAuth{authenticated: true}, nil)
	req := httptest.NewRequest(http.MethodGet, "/login/status", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "true") {
		t.Errorf("expected 'true' in body, got: %s", w.Body.String())
	}
}

func TestHandleLoginStatus_unauthenticated(t *testing.T) {
	srv := newTestServer(t, fakeAuth{authenticated: false}, nil)
	req := httptest.NewRequest(http.MethodGet, "/login/status", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "false") {
		t.Errorf("expected 'false' in body, got: %s", w.Body.String())
	}
}

// --- handleDoctor ---

func newDoctorServer(t *testing.T, auth authService, gc graphService, lc llmService, st *store.Store) *Server {
	t.Helper()
	s := &Server{
		cfg:           config.Default(),
		mux:           http.NewServeMux(),
		auth:          auth,
		client:        gc,
		llm:           lc,
		store:         st,
		pendingLogins: make(map[string]pendingLogin),
	}
	s.routes()
	return s
}

func TestHandleDoctor_allOK(t *testing.T) {
	st := newMemStore(t)
	gc := fakeGraph{
		msgs:   []graph.Message{{ID: "m1", Subject: "Hello"}},
		events: []graph.Event{{ID: "e1", Subject: "Standup"}},
	}
	lc := fakeLLM{reply: "pong"}
	srv := newDoctorServer(t, fakeAuth{authenticated: true}, gc, lc, st)

	req := httptest.NewRequest(http.MethodGet, "/doctor", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// All four checks should show OK
	if strings.Count(body, `class="ok"`) < 4 {
		t.Errorf("expected 4 ok checks in body, got:\n%s", body)
	}
	if strings.Contains(body, `class="fail"`) {
		t.Errorf("expected no fail checks in body, got:\n%s", body)
	}
}

func TestHandleDoctor_graphFail(t *testing.T) {
	st := newMemStore(t)
	gc := fakeGraph{
		msgsErr: fmt.Errorf("token expired"),
		evtsErr: fmt.Errorf("token expired"),
	}
	lc := fakeLLM{reply: "pong"}
	srv := newDoctorServer(t, fakeAuth{authenticated: true}, gc, lc, st)

	req := httptest.NewRequest(http.MethodGet, "/doctor", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// Graph mail and calendar should both fail
	if strings.Count(body, `class="fail"`) < 2 {
		t.Errorf("expected at least 2 fail checks for graph errors, got:\n%s", body)
	}
	if !strings.Contains(body, "token expired") {
		t.Errorf("expected error detail in body, got:\n%s", body)
	}
}

func TestHandleDoctor_notAuthenticated(t *testing.T) {
	st := newMemStore(t)
	srv := newDoctorServer(t, fakeAuth{authenticated: false}, fakeGraph{}, fakeLLM{reply: "pong"}, st)

	req := httptest.NewRequest(http.MethodGet, "/doctor", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "not authenticated") {
		t.Errorf("expected 'not authenticated' detail for graph checks, got:\n%s", body)
	}
}

func TestHandleDoctor_llmNotConfigured(t *testing.T) {
	st := newMemStore(t)
	srv := newDoctorServer(t, fakeAuth{authenticated: true}, fakeGraph{}, nil, st)

	req := httptest.NewRequest(http.MethodGet, "/doctor", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "not configured") {
		t.Errorf("expected 'not configured' detail for LLM, got:\n%s", body)
	}
}

func TestHandleDoctor_llmFail(t *testing.T) {
	st := newMemStore(t)
	gc := fakeGraph{
		msgs:   []graph.Message{{ID: "m1"}},
		events: []graph.Event{{ID: "e1"}},
	}
	lc := fakeLLM{err: fmt.Errorf("rate limit exceeded")}
	srv := newDoctorServer(t, fakeAuth{authenticated: true}, gc, lc, st)

	req := httptest.NewRequest(http.MethodGet, "/doctor", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "rate limit exceeded") {
		t.Errorf("expected error detail in body, got:\n%s", body)
	}
}

// --- buildSystemPrompt ---

func TestBuildSystemPrompt_noOverall(t *testing.T) {
	got := buildSystemPrompt("", "be brief")
	if got != "be brief" {
		t.Errorf("expected specific prompt unchanged, got %q", got)
	}
}

func TestBuildSystemPrompt_withOverall(t *testing.T) {
	got := buildSystemPrompt("always respond in French", "be brief")
	want := "always respond in French\n\nbe brief"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildSystemPrompt_overallOnly(t *testing.T) {
	// Specific prompt can be empty (degenerate case; should still work).
	got := buildSystemPrompt("global rule", "")
	want := "global rule\n\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildSystemPrompt_bothEmpty(t *testing.T) {
	got := buildSystemPrompt("", "")
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}
