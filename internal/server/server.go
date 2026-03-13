package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/darrint/officeagent/internal/config"
	"github.com/darrint/officeagent/internal/graph"
	"github.com/darrint/officeagent/internal/llm"
	"github.com/darrint/officeagent/internal/store"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"golang.org/x/oauth2"
)

// authService is the subset of graph.Auth used by the server.
// Using an interface here makes handlers testable without a real OAuth flow.
type authService interface {
	IsAuthenticated(ctx context.Context) bool
	AuthCodeURL(redirectURI string) (authURL, state, verifier string, err error)
	ExchangeCode(ctx context.Context, code, verifier, redirectURI string) (*oauth2.Token, error)
}

// graphService is the subset of graph.Client used by the server.
type graphService interface {
	ListMessages(ctx context.Context, top int) ([]graph.Message, error)
	ListEvents(ctx context.Context, top int) ([]graph.Event, error)
}

// llmService is the subset of llm.Client used by the server.
type llmService interface {
	Chat(ctx context.Context, messages []llm.Message) (string, error)
}

// Default system prompts. Used when no custom prompt is stored.
const (
	defaultEmailPrompt    = "You are a helpful executive assistant. Give the user a concise summary of their recent inbox. Highlight anything urgent or requiring action. Be friendly but brief."
	defaultCalendarPrompt = "You are a helpful executive assistant. Give the user a concise morning briefing of their upcoming calendar events. Be friendly but brief."
)

// easternLoc is the America/New_York timezone, loaded once at startup.
// Falls back to UTC if the IANA database is unavailable (shouldn't happen
// given the time/tzdata blank import in main).
var easternLoc = func() *time.Location {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		log.Printf("server: could not load America/New_York timezone, falling back to UTC: %v", err)
		return time.UTC
	}
	return loc
}()

// pendingLogin holds PKCE state for an in-flight authorization.
type pendingLogin struct {
	verifier string
	expiry   time.Time
}

// Server is the officeagent HTTP server.
type Server struct {
	cfg    *config.Config
	mux    *http.ServeMux
	auth   authService
	client graphService
	llm    llmService
	store  *store.Store

	pendingMu     sync.Mutex
	pendingLogins map[string]pendingLogin // state -> pending
}

// New creates a new Server with routes registered.
func New(cfg *config.Config, auth *graph.Auth, client *graph.Client, llmClient *llm.Client, st *store.Store) *Server {
	s := &Server{
		cfg:           cfg,
		mux:           http.NewServeMux(),
		auth:          auth,
		client:        client,
		store:         st,
		pendingLogins: make(map[string]pendingLogin),
	}
	// Assign llmClient only when non-nil to preserve nil interface semantics.
	// A typed nil (*llm.Client)(nil) assigned to an interface field would make
	// the field non-nil, causing spurious "LLM not configured" paths to panic.
	if llmClient != nil {
		s.llm = llmClient
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /", s.handleSummaryPage)
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("GET /doctor", s.handleDoctor)
	s.mux.HandleFunc("GET /login", s.handleLogin)
	s.mux.HandleFunc("GET /login/callback", s.handleLoginCallback)
	s.mux.HandleFunc("GET /login/status", s.handleLoginStatus)
	s.mux.HandleFunc("GET /settings", s.handleSettingsGet)
	s.mux.HandleFunc("POST /settings", s.handleSettingsPost)
	s.mux.HandleFunc("POST /feedback", s.handleFeedback)
	s.mux.HandleFunc("GET /api/mail", s.handleMail)
	s.mux.HandleFunc("GET /api/calendar", s.handleCalendar)
	s.mux.HandleFunc("GET /api/llm/ping", s.handleLLMPing)
}

// Run starts the HTTP server and blocks until it returns an error.
func (s *Server) Run() error {
	log.Printf("officeagent listening on %s", s.cfg.Addr)
	return http.ListenAndServe(s.cfg.Addr, s.mux)
}

// --- handlers ---

var summaryTmpl = template.Must(template.New("summary").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>officeagent</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:system-ui,sans-serif;background:#f5f5f5;color:#1a1a1a;padding:2rem 1rem;line-height:1.6}
.wrap{max-width:720px;margin:0 auto}
header{display:flex;align-items:baseline;gap:1rem;margin-bottom:2rem;border-bottom:2px solid #0078d4;padding-bottom:.75rem}
header h1{font-size:1.4rem;color:#0078d4;font-weight:700;letter-spacing:-.5px}
header span{font-size:.85rem;color:#666;flex:1}
header a{font-size:.82rem;color:#888;text-decoration:none}
header a:hover{color:#0078d4}
.section{margin-bottom:1.5rem}
.section-title{font-size:.75rem;font-weight:700;letter-spacing:.08em;text-transform:uppercase;color:#888;margin-bottom:.5rem}
.card{background:#fff;border-radius:10px;padding:1.75rem 2rem;box-shadow:0 1px 4px rgba(0,0,0,.08)}
.card h2,.card h3{margin:1.2em 0 .4em;font-size:1.05rem;color:#0078d4}
.card h2:first-child,.card h3:first-child,.card p:first-child{margin-top:0}
.card p{margin:.6em 0}
.card ul,.card ol{margin:.6em 0 .6em 1.4em}
.card li{margin:.25em 0}
.card strong{font-weight:600}
.card em{font-style:italic}
.card code{background:#f0f0f0;padding:.1em .35em;border-radius:3px;font-size:.9em;font-family:monospace}
.card pre{background:#f0f0f0;padding:1em;border-radius:6px;overflow-x:auto;font-size:.88em;line-height:1.5}
.card pre code{background:none;padding:0}
.card hr{border:none;border-top:1px solid #e8e8e8;margin:1.2em 0}
.error{color:#c00;background:#fff0f0;border:1px solid #fcc;border-radius:8px;padding:1rem 1.25rem}
details{margin-top:1.5rem}
details summary{cursor:pointer;font-size:.82rem;color:#888;user-select:none;padding:.4rem 0}
details summary:hover{color:#555}
details pre{background:#1e1e1e;color:#d4d4d4;padding:1.25rem;border-radius:8px;font-size:.8rem;line-height:1.5;overflow-x:auto;margin-top:.5rem;white-space:pre-wrap;word-break:break-all}
.feedback{display:flex;align-items:center;gap:.5rem;margin-top:.75rem;flex-wrap:wrap}
.feedback button{background:none;border:1px solid #ddd;border-radius:6px;padding:.3rem .7rem;font-size:1rem;cursor:pointer;line-height:1}
.feedback button:hover{border-color:#0078d4;background:#f0f6ff}
.feedback input[type=text]{flex:1;min-width:160px;padding:.3rem .6rem;font-size:.82rem;border:1px solid #ddd;border-radius:6px;color:#1a1a1a}
.feedback input[type=text]:focus{outline:none;border-color:#0078d4}
</style>
</head>
<body>
<div class="wrap">
  <header>
    <h1>officeagent</h1>
    <span>morning briefing</span>
    <a href="/settings">Settings</a>
  </header>
  {{if .FatalError}}
  <div class="error">{{.FatalError}}</div>
  {{else}}
  <div class="section">
    <div class="section-title">Email</div>
    {{if .Email.Error}}<div class="error">{{.Email.Error}}</div>
    {{else}}
    <div class="card">{{.Email.HTML}}</div>
    <form method="POST" action="/feedback" class="feedback">
      <input type="hidden" name="section" value="email">
      <button type="submit" name="rating" value="good" title="Helpful">👍</button>
      <button type="submit" name="rating" value="bad" title="Needs improvement">👎</button>
      <input type="text" name="note" placeholder="What should be different? (optional)" maxlength="300">
    </form>
    {{end}}
  </div>
  <div class="section">
    <div class="section-title">Calendar</div>
    {{if .Calendar.Error}}<div class="error">{{.Calendar.Error}}</div>
    {{else}}
    <div class="card">{{.Calendar.HTML}}</div>
    <form method="POST" action="/feedback" class="feedback">
      <input type="hidden" name="section" value="calendar">
      <button type="submit" name="rating" value="good" title="Helpful">👍</button>
      <button type="submit" name="rating" value="bad" title="Needs improvement">👎</button>
      <input type="text" name="note" placeholder="What should be different? (optional)" maxlength="300">
    </form>
    {{end}}
  </div>
  <details>
    <summary>Raw JSON</summary>
    <pre>{{.RawJSON}}</pre>
  </details>
  {{end}}
</div>
</body>
</html>`))

type sectionData struct {
	HTML template.HTML
	Raw  string // raw LLM reply, for the JSON block
	Error string
}

type pageData struct {
	Email      sectionData
	Calendar   sectionData
	RawJSON    string
	FatalError string
}

// mdRenderer is a goldmark instance with GFM extensions (tables, strikethrough,
// task lists, autolinks). Created once at startup; goldmark is concurrency-safe.
var mdRenderer = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
)

// renderMarkdown converts a markdown string to HTML. On error it falls back
// to an escaped <pre> block.
func renderMarkdown(src string) template.HTML {
	var buf bytes.Buffer
	if err := mdRenderer.Convert([]byte(src), &buf); err != nil {
		log.Printf("goldmark: %v", err)
		return template.HTML("<pre>" + template.HTMLEscapeString(src) + "</pre>") //nolint:gosec
	}
	return template.HTML(buf.String()) //nolint:gosec // goldmark output is safe HTML
}

// getPrompt returns the stored system prompt for key, or defaultVal if not set.
func (s *Server) getPrompt(key, defaultVal string) string {
	if s.store == nil {
		return defaultVal
	}
	val, err := s.store.Get("prompt." + key)
	if err != nil {
		log.Printf("store get prompt.%s: %v", key, err)
		return defaultVal
	}
	if val == "" {
		return defaultVal
	}
	return val
}

var settingsTmpl = template.Must(template.New("settings").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>officeagent — Settings</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:system-ui,sans-serif;background:#f5f5f5;color:#1a1a1a;padding:2rem 1rem;line-height:1.6}
.wrap{max-width:720px;margin:0 auto}
header{display:flex;align-items:baseline;gap:1rem;margin-bottom:2rem;border-bottom:2px solid #0078d4;padding-bottom:.75rem}
header h1{font-size:1.4rem;color:#0078d4;font-weight:700;letter-spacing:-.5px}
header a{font-size:.82rem;color:#888;text-decoration:none}
header a:hover{color:#0078d4}
.card{background:#fff;border-radius:10px;padding:1.75rem 2rem;box-shadow:0 1px 4px rgba(0,0,0,.08);margin-bottom:1.5rem}
label{display:block;font-size:.78rem;font-weight:700;letter-spacing:.06em;text-transform:uppercase;color:#555;margin-bottom:.5rem}
textarea{width:100%;min-height:100px;padding:.75rem;font-family:system-ui,sans-serif;font-size:.9rem;line-height:1.5;border:1px solid #ddd;border-radius:6px;resize:vertical;color:#1a1a1a}
textarea:focus{outline:none;border-color:#0078d4;box-shadow:0 0 0 2px rgba(0,120,212,.15)}
.hint{font-size:.78rem;color:#888;margin-top:.35rem}
.actions{display:flex;gap:.75rem;align-items:center;margin-top:1.5rem}
button{background:#0078d4;color:#fff;border:none;border-radius:6px;padding:.6rem 1.4rem;font-size:.9rem;font-weight:600;cursor:pointer}
button:hover{background:#006cbd}
.saved{color:#107c10;font-size:.88rem;font-weight:600}
</style>
</head>
<body>
<div class="wrap">
  <header>
    <h1>officeagent</h1>
    <a href="/">← Morning briefing</a>
  </header>
  <form method="POST" action="/settings">
    <div class="card">
      <label for="email_prompt">Email summary prompt</label>
      <textarea id="email_prompt" name="email_prompt" rows="4">{{.EmailPrompt}}</textarea>
      <p class="hint">This system prompt is sent to the LLM when summarizing your inbox.</p>
    </div>
    <div class="card">
      <label for="calendar_prompt">Calendar summary prompt</label>
      <textarea id="calendar_prompt" name="calendar_prompt" rows="4">{{.CalendarPrompt}}</textarea>
      <p class="hint">This system prompt is sent to the LLM when summarizing your calendar.</p>
    </div>
    <div class="actions">
      <button type="submit">Save prompts</button>
      {{if .Saved}}<span class="saved">&#10003; Saved</span>{{end}}
    </div>
  </form>
</div>
</body>
</html>`))

type settingsData struct {
	EmailPrompt    string
	CalendarPrompt string
	Saved          bool
}

func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	if !s.auth.IsAuthenticated(r.Context()) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	data := settingsData{
		EmailPrompt:    s.getPrompt("email", defaultEmailPrompt),
		CalendarPrompt: s.getPrompt("calendar", defaultCalendarPrompt),
		Saved:          r.URL.Query().Get("saved") == "1",
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := settingsTmpl.Execute(w, data); err != nil {
		log.Printf("settings template: %v", err)
	}
}

func (s *Server) handleSettingsPost(w http.ResponseWriter, r *http.Request) {
	if !s.auth.IsAuthenticated(r.Context()) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form data", http.StatusBadRequest)
		return
	}
	emailPrompt := strings.TrimSpace(r.FormValue("email_prompt"))
	calPrompt := strings.TrimSpace(r.FormValue("calendar_prompt"))

	if s.store != nil {
		if err := s.store.Set("prompt.email", emailPrompt); err != nil {
			log.Printf("store set prompt.email: %v", err)
		}
		if err := s.store.Set("prompt.calendar", calPrompt); err != nil {
			log.Printf("store set prompt.calendar: %v", err)
		}
	}
	http.Redirect(w, r, "/settings?saved=1", http.StatusSeeOther)
}

// feedbackContext returns a string summarising recent feedback for the given
// section, to be appended to the system prompt so the LLM can self-correct.
func (s *Server) feedbackContext(section string) string {
	if s.store == nil {
		return ""
	}
	entries, err := s.store.RecentFeedback(section, 5)
	if err != nil || len(entries) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n\nRecent user feedback on your previous summaries (newest first):\n")
	for _, f := range entries {
		icon := "👍"
		if f.Rating == "bad" {
			icon = "👎"
		}
		if f.Note != "" {
			fmt.Fprintf(&sb, "- %s %s\n", icon, f.Note)
		} else {
			fmt.Fprintf(&sb, "- %s (no additional comment)\n", icon)
		}
	}
	sb.WriteString("Use this feedback to adjust your tone, length, and focus accordingly.")
	return sb.String()
}

func (s *Server) handleFeedback(w http.ResponseWriter, r *http.Request) {
	if !s.auth.IsAuthenticated(r.Context()) {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form data", http.StatusBadRequest)
		return
	}
	section := r.FormValue("section")
	rating := r.FormValue("rating")
	note := strings.TrimSpace(r.FormValue("note"))

	if section != "email" && section != "calendar" {
		http.Error(w, "invalid section", http.StatusBadRequest)
		return
	}
	if rating != "good" && rating != "bad" {
		http.Error(w, "invalid rating", http.StatusBadRequest)
		return
	}
	if s.store != nil {
		if err := s.store.AddFeedback(section, rating, note); err != nil {
			log.Printf("store add feedback: %v", err)
		}
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleSummaryPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if !s.auth.IsAuthenticated(r.Context()) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	servePage := func(data pageData) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := summaryTmpl.Execute(w, data); err != nil {
			log.Printf("summary template: %v", err)
		}
	}

	if s.llm == nil {
		servePage(pageData{FatalError: "LLM not configured — set GITHUB_TOKEN"})
		return
	}

	// Fetch email and calendar data concurrently.
	type emailResult struct {
		section sectionData
	}
	type calResult struct {
		section sectionData
	}

	emailCh := make(chan emailResult, 1)
	calCh := make(chan calResult, 1)

	go func() {
		msgs, err := s.client.ListMessages(r.Context(), 20)
		if err != nil {
			emailCh <- emailResult{sectionData{Error: fmt.Sprintf("Failed to fetch email: %v", err)}}
			return
		}
		var sb strings.Builder
		if len(msgs) == 0 {
			sb.WriteString("No recent messages.")
		} else {
			for _, m := range msgs {
				fmt.Fprintf(&sb, "- From: %s | Subject: %s | Received: %s\n  Preview: %s\n",
					m.From,
					m.Subject,
					m.ReceivedAt.In(easternLoc).Format("Mon Jan 2 3:04 PM MST"),
					m.BodyPreview,
				)
			}
		}
		reply, err := s.llm.Chat(r.Context(), []llm.Message{
			{
				Role:    "system",
				Content: s.getPrompt("email", defaultEmailPrompt) + s.feedbackContext("email"),
			},
			{Role: "user", Content: "Here are my recent emails:\n\n" + sb.String()},
		})
		if err != nil {
			emailCh <- emailResult{sectionData{Error: fmt.Sprintf("LLM error (email): %v", err)}}
			return
		}
		emailCh <- emailResult{sectionData{HTML: renderMarkdown(reply), Raw: reply}}
	}()

	go func() {
		events, err := s.client.ListEvents(r.Context(), 20)
		if err != nil {
			calCh <- calResult{sectionData{Error: fmt.Sprintf("Failed to fetch calendar: %v", err)}}
			return
		}
		var sb strings.Builder
		if len(events) == 0 {
			sb.WriteString("No upcoming events.")
		} else {
			for _, e := range events {
				fmt.Fprintf(&sb, "- %s: %s to %s\n",
					e.Subject,
					e.Start.In(easternLoc).Format("Mon Jan 2 3:04 PM MST"),
					e.End.In(easternLoc).Format("3:04 PM MST"),
				)
			}
		}
		reply, err := s.llm.Chat(r.Context(), []llm.Message{
			{
				Role:    "system",
				Content: s.getPrompt("calendar", defaultCalendarPrompt) + s.feedbackContext("calendar"),
			},
			{Role: "user", Content: "Here are my upcoming calendar events:\n\n" + sb.String()},
		})
		if err != nil {
			calCh <- calResult{sectionData{Error: fmt.Sprintf("LLM error (calendar): %v", err)}}
			return
		}
		calCh <- calResult{sectionData{HTML: renderMarkdown(reply), Raw: reply}}
	}()

	emailRes := <-emailCh
	calRes := <-calCh

	rawJSON, _ := json.MarshalIndent(map[string]string{
		"email":    emailRes.section.Raw,
		"calendar": calRes.section.Raw,
	}, "", "  ")

	servePage(pageData{
		Email:    emailRes.section,
		Calendar: calRes.section,
		RawJSON:  string(rawJSON),
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"status": "ok"})
}

var loginTmpl = template.Must(template.New("login").Parse(`<!DOCTYPE html>
<html>
<head><meta charset="utf-8"><title>officeagent — Login</title>
<style>body{font-family:sans-serif;max-width:600px;margin:4rem auto;padding:0 1rem}
a{color:#0078d4;font-size:1.1em}.card{border:1px solid #ddd;border-radius:8px;padding:2rem;margin-top:2rem}</style>
</head>
<body>
{{if .Authenticated}}
<h1>&#10003; Signed in</h1>
<p>You are authenticated with Microsoft Graph.</p>
<ul>
  <li><a href="/api/mail">View recent mail (JSON)</a></li>
  <li><a href="/api/calendar">View upcoming calendar events (JSON)</a></li>
</ul>
{{else if .ClientIDMissing}}
<h1>Configuration required</h1>
<p>Set the <code>OFFICEAGENT_CLIENT_ID</code> environment variable to your Azure AD app client ID and restart.</p>
{{else}}
<h1>Sign in to Microsoft</h1>
<div class="card">
  <p><a href="/login">Sign in with your Microsoft account</a></p>
</div>
{{end}}
</body>
</html>`))

type loginData struct {
	Authenticated   bool
	ClientIDMissing bool
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.cfg.AzureClientID == "" {
		if err := loginTmpl.Execute(w, loginData{ClientIDMissing: true}); err != nil {
			log.Printf("login template: %v", err)
		}
		return
	}

	if s.auth.IsAuthenticated(r.Context()) {
		if err := loginTmpl.Execute(w, loginData{Authenticated: true}); err != nil {
			log.Printf("login template: %v", err)
		}
		return
	}

	authURL, state, verifier, err := s.auth.AuthCodeURL(s.cfg.RedirectURI)
	if err != nil {
		http.Error(w, fmt.Sprintf("generate auth URL: %v", err), http.StatusInternalServerError)
		return
	}

	s.pendingMu.Lock()
	s.pendingLogins[state] = pendingLogin{
		verifier: verifier,
		expiry:   time.Now().Add(10 * time.Minute),
	}
	s.pendingMu.Unlock()

	http.Redirect(w, r, authURL, http.StatusFound)
}

func (s *Server) handleLoginCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	if errCode := q.Get("error"); errCode != "" {
		http.Error(w, fmt.Sprintf("auth error: %s — %s", errCode, q.Get("error_description")), http.StatusBadRequest)
		return
	}

	state := q.Get("state")
	code := q.Get("code")

	s.pendingMu.Lock()
	pending, ok := s.pendingLogins[state]
	if ok {
		delete(s.pendingLogins, state)
	}
	s.pendingMu.Unlock()

	if !ok || time.Now().After(pending.expiry) {
		http.Error(w, "invalid or expired state parameter — try signing in again", http.StatusBadRequest)
		return
	}

	tok, err := s.auth.ExchangeCode(r.Context(), code, pending.verifier, s.cfg.RedirectURI)
	if err != nil {
		http.Error(w, fmt.Sprintf("exchange code: %v", err), http.StatusInternalServerError)
		return
	}
	log.Printf("authenticated, token expires %s", tok.Expiry)

	http.Redirect(w, r, "/login", http.StatusFound)
}

func (s *Server) handleLoginStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"authenticated": s.auth.IsAuthenticated(r.Context()),
	})
}

func (s *Server) handleMail(w http.ResponseWriter, r *http.Request) {
	if !s.auth.IsAuthenticated(r.Context()) {
		http.Error(w, "not authenticated — visit /login", http.StatusUnauthorized)
		return
	}
	msgs, err := s.client.ListMessages(r.Context(), 20)
	if err != nil {
		http.Error(w, fmt.Sprintf("list messages: %v", err), http.StatusInternalServerError)
		return
	}
	writeJSON(w, msgs)
}

func (s *Server) handleCalendar(w http.ResponseWriter, r *http.Request) {
	if !s.auth.IsAuthenticated(r.Context()) {
		http.Error(w, "not authenticated — visit /login", http.StatusUnauthorized)
		return
	}
	events, err := s.client.ListEvents(r.Context(), 20)
	if err != nil {
		http.Error(w, fmt.Sprintf("list events: %v", err), http.StatusInternalServerError)
		return
	}
	writeJSON(w, events)
}

func (s *Server) handleLLMPing(w http.ResponseWriter, r *http.Request) {
	if s.llm == nil {
		http.Error(w, "LLM not configured — set GITHUB_TOKEN", http.StatusServiceUnavailable)
		return
	}
	reply, err := s.llm.Chat(r.Context(), []llm.Message{
		{Role: "user", Content: "Say hello in one sentence."},
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("llm chat: %v", err), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"reply": reply})
}

// --- doctor page ---

var doctorTmpl = template.Must(template.New("doctor").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>officeagent — Diagnostics</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:system-ui,sans-serif;background:#f5f5f5;color:#1a1a1a;padding:2rem 1rem;line-height:1.6}
.wrap{max-width:720px;margin:0 auto}
header{display:flex;align-items:baseline;gap:1rem;margin-bottom:2rem;border-bottom:2px solid #0078d4;padding-bottom:.75rem}
header h1{font-size:1.4rem;color:#0078d4;font-weight:700;letter-spacing:-.5px}
header a{font-size:.82rem;color:#888;text-decoration:none}
header a:hover{color:#0078d4}
table{width:100%;border-collapse:collapse;background:#fff;border-radius:10px;overflow:hidden;box-shadow:0 1px 4px rgba(0,0,0,.08)}
th{text-align:left;font-size:.72rem;font-weight:700;letter-spacing:.08em;text-transform:uppercase;color:#888;padding:.75rem 1rem;border-bottom:2px solid #f0f0f0}
td{padding:.75rem 1rem;border-bottom:1px solid #f5f5f5;font-size:.9rem;vertical-align:top}
tr:last-child td{border-bottom:none}
.ok{color:#107c10;font-weight:600}
.fail{color:#c00;font-weight:600}
.warn{color:#b86800;font-weight:600}
.detail{color:#555;font-size:.82rem}
.latency{color:#888;font-size:.82rem}
.ts{margin-top:1.25rem;font-size:.78rem;color:#aaa;text-align:right}
</style>
</head>
<body>
<div class="wrap">
  <header>
    <h1>officeagent</h1>
    <span style="flex:1">diagnostics</span>
    <a href="/">← Morning briefing</a>
  </header>
  <table>
    <thead><tr><th>System</th><th>Status</th><th>Latency</th><th>Detail</th></tr></thead>
    <tbody>
    {{range .Checks}}
    <tr>
      <td><strong>{{.Name}}</strong></td>
      <td class="{{.StatusClass}}">{{.StatusIcon}} {{.Status}}</td>
      <td class="latency">{{.LatencyStr}}</td>
      <td class="detail">{{.Detail}}</td>
    </tr>
    {{end}}
    </tbody>
  </table>
  <p class="ts">checked at {{.CheckedAt}}</p>
</div>
</body>
</html>`))

type checkResult struct {
	Name       string
	ok         bool
	Detail     string
	Latency    time.Duration
}

func (c checkResult) Status() string {
	if c.ok {
		return "OK"
	}
	return "FAIL"
}

func (c checkResult) StatusClass() string {
	if c.ok {
		return "ok"
	}
	return "fail"
}

func (c checkResult) StatusIcon() string {
	if c.ok {
		return "✓"
	}
	return "✗"
}

func (c checkResult) LatencyStr() string {
	if c.Latency < time.Millisecond {
		return fmt.Sprintf("%dµs", c.Latency.Microseconds())
	}
	return fmt.Sprintf("%dms", c.Latency.Milliseconds())
}

type doctorData struct {
	Checks    []checkResult
	CheckedAt string
}

func (s *Server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	type result struct {
		idx int
		cr  checkResult
	}
	checks := make([]checkResult, 4)
	ch := make(chan result, 4)

	// Check 0: SQLite store
	go func() {
		cr := checkResult{Name: "SQLite"}
		start := time.Now()
		if s.store == nil {
			cr.Detail = "store not initialised"
			ch <- result{0, cr}
			return
		}
		testKey := "__doctor_ping__"
		if err := s.store.Set(testKey, "1"); err != nil {
			cr.Detail = fmt.Sprintf("write failed: %v", err)
		} else if v, err := s.store.Get(testKey); err != nil {
			cr.Detail = fmt.Sprintf("read failed: %v", err)
		} else if v != "1" {
			cr.Detail = fmt.Sprintf("read/write mismatch: got %q", v)
		} else {
			cr.ok = true
			cr.Detail = "read/write roundtrip passed"
		}
		cr.Latency = time.Since(start)
		ch <- result{0, cr}
	}()

	// Check 1: Microsoft Graph — mail access
	go func() {
		cr := checkResult{Name: "Graph (mail)"}
		start := time.Now()
		if !s.auth.IsAuthenticated(ctx) {
			cr.Detail = "not authenticated — visit /login"
			cr.Latency = time.Since(start)
			ch <- result{1, cr}
			return
		}
		msgs, err := s.client.ListMessages(ctx, 1)
		cr.Latency = time.Since(start)
		if err != nil {
			cr.Detail = fmt.Sprintf("ListMessages failed: %v", err)
		} else {
			cr.ok = true
			cr.Detail = fmt.Sprintf("OK (%d message(s) accessible)", len(msgs))
		}
		ch <- result{1, cr}
	}()

	// Check 2: Microsoft Graph — calendar access
	go func() {
		cr := checkResult{Name: "Graph (calendar)"}
		start := time.Now()
		if !s.auth.IsAuthenticated(ctx) {
			cr.Detail = "not authenticated — visit /login"
			cr.Latency = time.Since(start)
			ch <- result{2, cr}
			return
		}
		events, err := s.client.ListEvents(ctx, 1)
		cr.Latency = time.Since(start)
		if err != nil {
			cr.Detail = fmt.Sprintf("ListEvents failed: %v", err)
		} else {
			cr.ok = true
			cr.Detail = fmt.Sprintf("OK (%d event(s) accessible)", len(events))
		}
		ch <- result{2, cr}
	}()

	// Check 3: GitHub Copilot LLM
	go func() {
		cr := checkResult{Name: "LLM (GitHub Copilot)"}
		start := time.Now()
		if s.llm == nil {
			cr.Detail = "not configured — set GITHUB_TOKEN"
			cr.Latency = time.Since(start)
			ch <- result{3, cr}
			return
		}
		_, err := s.llm.Chat(ctx, []llm.Message{
			{Role: "user", Content: "Respond with exactly the word: pong"},
		})
		cr.Latency = time.Since(start)
		if err != nil {
			cr.Detail = fmt.Sprintf("chat failed: %v", err)
		} else {
			cr.ok = true
			cr.Detail = "chat completion responded"
		}
		ch <- result{3, cr}
	}()

	for i := 0; i < 4; i++ {
		r := <-ch
		checks[r.idx] = r.cr
	}

	data := doctorData{
		Checks:    checks,
		CheckedAt: time.Now().In(easternLoc).Format("Mon Jan 2 2006 3:04:05 PM MST"),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := doctorTmpl.Execute(w, data); err != nil {
		log.Printf("doctor template: %v", err)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writeJSON: %v", err)
	}
}
