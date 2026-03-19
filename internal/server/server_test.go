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
	"time"

	"github.com/darrint/officeagent/internal/config"
	ghpkg "github.com/darrint/officeagent/internal/github"
	"github.com/darrint/officeagent/internal/graph"
	"github.com/darrint/officeagent/internal/llm"
	"github.com/darrint/officeagent/internal/store"
	"golang.org/x/oauth2"
)

// --- fakes ---

// fakeAuth implements authService. Authenticated controls IsAuthenticated.
type fakeAuth struct {
	authenticated bool
	expiry        time.Time // zero means no expiry info
	clearErr      error
	clearCalled   bool
}

func (f *fakeAuth) IsAuthenticated(_ context.Context) bool { return f.authenticated }
func (f *fakeAuth) AuthCodeURL(_ string) (string, string, string, error) {
	return "https://login.example.com/auth", "state", "verifier", nil
}
func (f *fakeAuth) ExchangeCode(_ context.Context, _, _, _ string) (*oauth2.Token, error) {
	return &oauth2.Token{AccessToken: "tok"}, nil
}
func (f *fakeAuth) TokenExpiry(_ context.Context) (time.Time, bool) {
	if f.expiry.IsZero() {
		return time.Time{}, false
	}
	return f.expiry, true
}
func (f *fakeAuth) ClearToken() error {
	f.clearCalled = true
	return f.clearErr
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
func (f fakeGraph) WriteFile(_ context.Context, _, _ string, _ []byte) (graph.DriveItem, error) {
	return graph.DriveItem{}, nil
}

// fakeLLM implements llmService.
type fakeLLM struct {
	reply string
	err   error
}

func (f fakeLLM) Chat(_ context.Context, _ []llm.Message) (string, error) {
	return f.reply, f.err
}

// fakeGitHub implements githubService.
type fakeGitHub struct {
	prs []ghpkg.PullRequest
	err error
}

func (f fakeGitHub) ListRecentPRs(_ context.Context, _ time.Time, _ []string, _ string) ([]ghpkg.PullRequest, error) {
	return f.prs, f.err
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
		progress:      newProgressBus(),
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
	srv := newTestServer(t, &fakeAuth{authenticated: true}, nil)
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
	srv := newTestServer(t, &fakeAuth{authenticated: true}, newMemStore(t))
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
	srv := newTestServer(t, &fakeAuth{authenticated: true}, newMemStore(t))
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
	srv := newTestServer(t, &fakeAuth{authenticated: false}, newMemStore(t))
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
	srv := newTestServer(t, &fakeAuth{authenticated: true}, st)
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
	srv := newTestServer(t, &fakeAuth{authenticated: true}, nil)
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
	srv := newTestServer(t, &fakeAuth{authenticated: false}, nil)
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
	srv := newDoctorServer(t, &fakeAuth{authenticated: true}, gc, lc, st)

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
	srv := newDoctorServer(t, &fakeAuth{authenticated: true}, gc, lc, st)

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
	srv := newDoctorServer(t, &fakeAuth{authenticated: false}, fakeGraph{}, fakeLLM{reply: "pong"}, st)

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
	srv := newDoctorServer(t, &fakeAuth{authenticated: true}, fakeGraph{}, nil, st)

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
	srv := newDoctorServer(t, &fakeAuth{authenticated: true}, gc, lc, st)

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

// --- lastWorkDaySince ---

func TestLastWorkDaySince_monday(t *testing.T) {
	// Monday → should return the previous Friday
	mon := time.Date(2026, 3, 9, 9, 0, 0, 0, time.UTC) // Monday
	got := lastWorkDaySince(mon)
	if got.Weekday() != time.Friday {
		t.Errorf("expected Friday, got %s", got.Weekday())
	}
	if got.Day() != 6 {
		t.Errorf("expected day 6 (Fri Mar 6), got %d", got.Day())
	}
}

func TestLastWorkDaySince_tuesday(t *testing.T) {
	tue := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC) // Tuesday
	got := lastWorkDaySince(tue)
	if got.Weekday() != time.Monday {
		t.Errorf("expected Monday, got %s", got.Weekday())
	}
}

func TestLastWorkDaySince_wednesday(t *testing.T) {
	wed := time.Date(2026, 3, 11, 9, 0, 0, 0, time.UTC) // Wednesday
	got := lastWorkDaySince(wed)
	if got.Weekday() != time.Tuesday {
		t.Errorf("expected Tuesday, got %s", got.Weekday())
	}
}

// --- handleSummaryPage / handleGenerate ---

func newSummaryServer(t *testing.T, auth authService, gc graphService, lc llmService, ghc githubService, st *store.Store) *Server {
	t.Helper()
	s := &Server{
		cfg:           config.Default(),
		mux:           http.NewServeMux(),
		auth:          auth,
		client:        gc,
		llm:           lc,
		ghClient:      ghc,
		store:         st,
		pendingLogins: make(map[string]pendingLogin),
		progress:      newProgressBus(),
	}
	s.routes()
	return s
}

func TestHandleSummaryPage_emptyState(t *testing.T) {
	// GET / with no cached report → empty state + Generate button, no API calls.
	st := newMemStore(t)
	lc := fakeLLM{reply: "should not be called"}
	srv := newSummaryServer(t, &fakeAuth{authenticated: true}, fakeGraph{}, lc, nil, st)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Generate Briefing") {
		t.Errorf("expected Generate Briefing button in empty state, got:\n%s", body)
	}
	if strings.Contains(body, "Email") || strings.Contains(body, "Calendar") {
		t.Errorf("expected no section content in empty state")
	}
}

func TestHandleGenerate_storesAndRedirects(t *testing.T) {
	// POST /generate → kicks off background generation, redirects to /generating.
	// We then call GenerateBriefing directly to populate the cache and verify
	// that GET / renders from it.
	st := newMemStore(t)
	gc := fakeGraph{
		msgs:   []graph.Message{{ID: "m1", Subject: "Hello"}},
		events: []graph.Event{{ID: "e1", Subject: "Standup"}},
	}
	lc := fakeLLM{reply: "LLM summary"}
	srv := newSummaryServer(t, &fakeAuth{authenticated: true}, gc, lc, nil, st)

	// Trigger generation via HTTP — should redirect to /generating.
	req := httptest.NewRequest(http.MethodPost, "/generate", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect after generate, got %d", w.Code)
	}
	if w.Header().Get("Location") != "/generating" {
		t.Errorf("expected redirect to /generating, got %s", w.Header().Get("Location"))
	}

	// Populate the cache synchronously so we can test GET / without races.
	if _, err := srv.GenerateBriefing(context.Background()); err != nil {
		t.Fatalf("GenerateBriefing: %v", err)
	}

	// GET / should now show the cached report.
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	w2 := httptest.NewRecorder()
	srv.mux.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200 on GET /, got %d", w2.Code)
	}
	body := w2.Body.String()
	if !strings.Contains(body, "Generated") {
		t.Errorf("expected Generated timestamp in page, got:\n%s", body)
	}
	if !strings.Contains(body, "Regenerate") {
		t.Errorf("expected Regenerate button in page, got:\n%s", body)
	}
	if !strings.Contains(body, "LLM summary") {
		t.Errorf("expected LLM reply in page, got:\n%s", body)
	}
}

func TestHandleGenerate_pageRefreshDoesNotRegenerate(t *testing.T) {
	// After a generate, repeated GET / calls must NOT call the LLM again.
	st := newMemStore(t)
	gc := fakeGraph{
		msgs:   []graph.Message{{ID: "m1", Subject: "Hello"}},
		events: []graph.Event{{ID: "e1", Subject: "Standup"}},
	}
	calls := 0
	lc := &callCountLLM{reply: "LLM summary", counter: &calls}
	srv := newSummaryServer(t, &fakeAuth{authenticated: true}, gc, lc, nil, st)

	// One generate (synchronous, to populate the cache reliably).
	if _, err := srv.GenerateBriefing(context.Background()); err != nil {
		t.Fatalf("GenerateBriefing: %v", err)
	}
	callsAfterGenerate := calls

	// Three page loads — should not increase call count.
	for i := 0; i < 3; i++ {
		srv.mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}
	if calls != callsAfterGenerate {
		t.Errorf("expected no additional LLM calls on page loads, got %d extra", calls-callsAfterGenerate)
	}
}

func TestHandleSummaryPage_githubSection(t *testing.T) {
	st := newMemStore(t)
	gc := fakeGraph{
		msgs:   []graph.Message{{ID: "m1", Subject: "Hello"}},
		events: []graph.Event{{ID: "e1", Subject: "Standup"}},
	}
	merged := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	ghc := fakeGitHub{prs: []ghpkg.PullRequest{
		{Number: 42, Title: "Fix the bug", Repo: "acme/backend", State: "closed", MergedAt: &merged, UpdatedAt: merged},
	}}
	lc := fakeLLM{reply: "Here is your GitHub PR summary."}
	srv := newSummaryServer(t, &fakeAuth{authenticated: true}, gc, lc, ghc, st)

	// Generate first.
	_, _ = srv.GenerateBriefing(context.Background())

	// Now GET / should show the GitHub section.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "GitHub PRs") {
		t.Errorf("expected GitHub PRs section heading in body")
	}
	if !strings.Contains(body, "GitHub PR summary") {
		t.Errorf("expected LLM reply in GitHub section, body: %s", body)
	}
}

func TestHandleSummaryPage_githubNotConfigured(t *testing.T) {
	st := newMemStore(t)
	gc := fakeGraph{
		msgs:   []graph.Message{{ID: "m1", Subject: "Hello"}},
		events: []graph.Event{{ID: "e1", Subject: "Standup"}},
	}
	lc := fakeLLM{reply: "summary"}
	srv := newSummaryServer(t, &fakeAuth{authenticated: true}, gc, lc, nil, st)

	_, _ = srv.GenerateBriefing(context.Background())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "GitHub PRs") {
		t.Errorf("expected GitHub PRs section to be absent when ghClient is nil")
	}
}

func TestHandleSummaryPage_githubError(t *testing.T) {
	st := newMemStore(t)
	gc := fakeGraph{
		msgs:   []graph.Message{{ID: "m1", Subject: "Hello"}},
		events: []graph.Event{{ID: "e1", Subject: "Standup"}},
	}
	ghc := fakeGitHub{err: fmt.Errorf("rate limit exceeded")}
	lc := fakeLLM{reply: "summary"}
	srv := newSummaryServer(t, &fakeAuth{authenticated: true}, gc, lc, ghc, st)

	_, _ = srv.GenerateBriefing(context.Background())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "rate limit exceeded") {
		t.Errorf("expected error detail in body, got:\n%s", w.Body.String())
	}
}

// callCountLLM is a fake LLM that counts how many times Chat is called.
type callCountLLM struct {
	reply   string
	counter *int
}

func (f *callCountLLM) Chat(_ context.Context, _ []llm.Message) (string, error) {
	*f.counter++
	return f.reply, nil
}

// fakeGraphMover extends fakeGraph with move support, implementing graphMoverService.
type fakeGraphMover struct {
	fakeGraph
	movedIDs     []string
	folderName   string
	folderErr    error
	moveErr      error
	fakeSkipped  int // how many IDs to report as skipped (not found)
}

func (f *fakeGraphMover) GetOrCreateFolder(_ context.Context, name string) (string, error) {
	f.folderName = name
	if f.folderErr != nil {
		return "", f.folderErr
	}
	return "folder-id", nil
}

func (f *fakeGraphMover) MoveMessages(_ context.Context, ids []string, _ string) (moved, skipped int, err error) {
	if f.moveErr != nil {
		return 0, 0, f.moveErr
	}
	// Simulate fakeSkipped IDs being not-found; the rest are moved.
	skipped = f.fakeSkipped
	if skipped > len(ids) {
		skipped = len(ids)
	}
	moved = len(ids) - skipped
	f.movedIDs = append(f.movedIDs, ids[skipped:]...)
	return moved, skipped, nil
}

// TestGenerateBriefing_cachesLowPrioIDs verifies that GenerateBriefing stores
// low-priority message IDs (returned by the LLM classifier) in the cached report.
func TestGenerateBriefing_cachesLowPrioIDs(t *testing.T) {
	st := newMemStore(t)
	gc := fakeGraph{
		msgs:   []graph.Message{{ID: "m1", From: "spam@example.com", Subject: "Buy now!"}},
		events: []graph.Event{{ID: "e1", Subject: "Standup"}},
	}
	// LLM returns "m1" as the low-priority ID for every chat call.
	lc := fakeLLM{reply: `["m1"]`}
	srv := newSummaryServer(t, &fakeAuth{authenticated: true}, gc, lc, nil, st)

	rep, err := srv.GenerateBriefing(context.Background())
	if err != nil {
		t.Fatalf("GenerateBriefing: %v", err)
	}
	if len(rep.LowPrioMsgs) == 0 {
		t.Fatal("expected at least one low-priority message cached")
	}
	found := false
	for _, m := range rep.LowPrioMsgs {
		if m.ID == "m1" && m.Source == "graph" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected m1/graph in LowPrioMsgs, got %+v", rep.LowPrioMsgs)
	}
}

// TestArchiveGraphLowPrio_usesCachedIDs verifies that archiveGraphLowPrio reads
// IDs from the cached report instead of re-invoking the LLM classifier.
func TestArchiveGraphLowPrio_usesCachedIDs(t *testing.T) {
	st := newMemStore(t)
	now := time.Now().UTC()

	// Pre-populate the cache with a low-prio message.
	rep := &cachedReport{
		LowPrioMsgs: []lowPrioMsg{
			{ID: "cached-id", Source: "graph", From: "spam@x.com", Subject: "Ad", ReceivedAt: now},
		},
		GeneratedAt: now,
	}
	if err := (&Server{store: st}).saveLastReport(rep); err != nil {
		t.Fatalf("saveLastReport: %v", err)
	}

	mover := &fakeGraphMover{
		fakeGraph: fakeGraph{
			msgs:   []graph.Message{{ID: "cached-id", Subject: "Ad", From: "spam@x.com"}},
			events: []graph.Event{},
		},
	}

	// LLM should NOT be called — use a counter to verify.
	callCount := 0
	lc := &callCountLLM{reply: "[]", counter: &callCount}
	srv := newSummaryServer(t, &fakeAuth{authenticated: true}, mover, lc, nil, st)

	n, _, errStr := srv.archiveGraphLowPrio(context.Background(), lc)
	if errStr != "" {
		t.Fatalf("archiveGraphLowPrio error: %s", errStr)
	}
	if n != 1 {
		t.Errorf("expected 1 message moved, got %d", n)
	}
	if callCount != 0 {
		t.Errorf("expected 0 LLM calls when cache is populated, got %d", callCount)
	}
	if len(mover.movedIDs) != 1 || mover.movedIDs[0] != "cached-id" {
		t.Errorf("expected cached-id to be moved, got %v", mover.movedIDs)
	}
}

// TestArchiveGraphLowPrio_reportsSkippedIDs verifies that stale (not-found) message
// IDs are counted and returned separately from moved IDs, without returning an error.
func TestArchiveGraphLowPrio_reportsSkippedIDs(t *testing.T) {
	st := newMemStore(t)
	now := time.Now().UTC()

	// Pre-populate cache with two IDs; one will be "not found" in the fake mover.
	rep := &cachedReport{
		LowPrioMsgs: []lowPrioMsg{
			{ID: "stale-id", Source: "graph", From: "gone@x.com", Subject: "Old ad", ReceivedAt: now},
			{ID: "live-id", Source: "graph", From: "spam@x.com", Subject: "New ad", ReceivedAt: now},
		},
		GeneratedAt: now,
	}
	if err := (&Server{store: st}).saveLastReport(rep); err != nil {
		t.Fatalf("saveLastReport: %v", err)
	}

	mover := &fakeGraphMover{
		fakeGraph: fakeGraph{
			msgs:   []graph.Message{},
			events: []graph.Event{},
		},
		fakeSkipped: 1, // first ID will be reported as not-found
	}
	lc := &callCountLLM{reply: "[]", counter: new(int)}
	srv := newSummaryServer(t, &fakeAuth{authenticated: true}, mover, lc, nil, st)

	moved, skipped, errStr := srv.archiveGraphLowPrio(context.Background(), lc)
	if errStr != "" {
		t.Fatalf("archiveGraphLowPrio error: %s", errStr)
	}
	if moved != 1 {
		t.Errorf("expected 1 moved, got %d", moved)
	}
	if skipped != 1 {
		t.Errorf("expected 1 skipped, got %d", skipped)
	}
}

// TestHandleSummaryPage_lowPrioSection verifies that the low-priority section
// appears in the page when the cached report contains low-prio messages.
func TestHandleSummaryPage_lowPrioSection(t *testing.T) {
	st := newMemStore(t)
	gc := fakeGraph{
		msgs:   []graph.Message{{ID: "m1", From: "spam@example.com", Subject: "Buy now!"}},
		events: []graph.Event{{ID: "e1", Subject: "Standup"}},
	}
	lc := fakeLLM{reply: `["m1"]`}
	srv := newSummaryServer(t, &fakeAuth{authenticated: true}, gc, lc, nil, st)

	_, _ = srv.GenerateBriefing(context.Background())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Low-priority mail") {
		t.Errorf("expected low-priority section in page body")
	}
	if !strings.Contains(body, "Buy now!") {
		t.Errorf("expected low-prio message subject in page body")
	}
	if !strings.Contains(body, "Move to Low-Priority Folder") {
		t.Errorf("expected move button in low-priority section")
	}
}

// --- handleDisconnect ---

func TestHandleDisconnect_clearsTokenAndRedirects(t *testing.T) {
	auth := &fakeAuth{authenticated: true}
	srv := newTestServer(t, auth, newMemStore(t))

	req := httptest.NewRequest(http.MethodPost, "/disconnect", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect, got %d", w.Code)
	}
	if w.Header().Get("Location") != "/connect" {
		t.Errorf("expected redirect to /connect, got %s", w.Header().Get("Location"))
	}
	if !auth.clearCalled {
		t.Error("expected ClearToken to be called")
	}
}

func TestHandleDisconnect_clearError(t *testing.T) {
	auth := &fakeAuth{authenticated: true, clearErr: fmt.Errorf("db error")}
	srv := newTestServer(t, auth, newMemStore(t))

	req := httptest.NewRequest(http.MethodPost, "/disconnect", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on clear error, got %d", w.Code)
	}
}

// --- handleConnect token expiry ---

func TestHandleConnect_showsExpiry(t *testing.T) {
	expiry := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	auth := &fakeAuth{authenticated: true, expiry: expiry}
	srv := newTestServer(t, auth, newMemStore(t))

	req := httptest.NewRequest(http.MethodGet, "/connect", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Token expires") {
		t.Errorf("expected token expiry text in connect page, got:\n%s", body)
	}
	// Disconnect button must appear when authenticated
	if !strings.Contains(body, "Disconnect") {
		t.Errorf("expected Disconnect button in connect page, got:\n%s", body)
	}
}

func TestHandleConnect_notAuthenticated_showsSignIn(t *testing.T) {
	auth := &fakeAuth{authenticated: false}
	st := newMemStore(t)
	_ = st.Set("setting.azure_client_id", "test-client-id")
	srv := newTestServer(t, auth, st)

	req := httptest.NewRequest(http.MethodGet, "/connect", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Sign in") {
		t.Errorf("expected Sign in link when not authenticated, got:\n%s", body)
	}
	if strings.Contains(body, "Disconnect") {
		t.Errorf("expected no Disconnect button when not authenticated")
	}
}
