package server

import (
	"bytes"
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
	"github.com/yuin/goldmark"
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
	auth   *graph.Auth
	client *graph.Client
	llm    *llm.Client

	pendingMu     sync.Mutex
	pendingLogins map[string]pendingLogin // state -> pending
}

// New creates a new Server with routes registered.
func New(cfg *config.Config, auth *graph.Auth, client *graph.Client, llmClient *llm.Client) *Server {
	s := &Server{
		cfg:           cfg,
		mux:           http.NewServeMux(),
		auth:          auth,
		client:        client,
		llm:           llmClient,
		pendingLogins: make(map[string]pendingLogin),
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /", s.handleSummaryPage)
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("GET /login", s.handleLogin)
	s.mux.HandleFunc("GET /login/callback", s.handleLoginCallback)
	s.mux.HandleFunc("GET /login/status", s.handleLoginStatus)
	s.mux.HandleFunc("GET /api/mail", s.handleMail)
	s.mux.HandleFunc("GET /api/calendar", s.handleCalendar)
	s.mux.HandleFunc("GET /api/calendar/summary", s.handleCalendarSummary)
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
header span{font-size:.85rem;color:#666}
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
</style>
</head>
<body>
<div class="wrap">
  <header>
    <h1>officeagent</h1>
    <span>morning briefing</span>
  </header>
  {{if .FatalError}}
  <div class="error">{{.FatalError}}</div>
  {{else}}
  <div class="section">
    <div class="section-title">Email</div>
    {{if .Email.Error}}<div class="error">{{.Email.Error}}</div>
    {{else}}<div class="card">{{.Email.HTML}}</div>{{end}}
  </div>
  <div class="section">
    <div class="section-title">Calendar</div>
    {{if .Calendar.Error}}<div class="error">{{.Calendar.Error}}</div>
    {{else}}<div class="card">{{.Calendar.HTML}}</div>{{end}}
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

// renderMarkdown converts a markdown string to HTML. On error it falls back
// to an escaped <pre> block.
func renderMarkdown(md string) template.HTML {
	var buf bytes.Buffer
	if err := goldmark.Convert([]byte(md), &buf); err != nil {
		log.Printf("goldmark: %v", err)
		return template.HTML("<pre>" + template.HTMLEscapeString(md) + "</pre>") //nolint:gosec
	}
	return template.HTML(buf.String()) //nolint:gosec // goldmark output is safe HTML
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
				Role: "system",
				Content: "You are a helpful executive assistant. " +
					"Give the user a concise summary of their recent inbox. " +
					"Highlight anything urgent or requiring action. Be friendly but brief.",
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
				Role: "system",
				Content: "You are a helpful executive assistant. " +
					"Give the user a concise morning briefing of their upcoming calendar events. " +
					"Be friendly but brief.",
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

func (s *Server) handleCalendarSummary(w http.ResponseWriter, r *http.Request) {
	if !s.auth.IsAuthenticated(r.Context()) {
		http.Error(w, "not authenticated — visit /login", http.StatusUnauthorized)
		return
	}
	if s.llm == nil {
		http.Error(w, "LLM not configured — set GITHUB_TOKEN", http.StatusServiceUnavailable)
		return
	}

	events, err := s.client.ListEvents(r.Context(), 20)
	if err != nil {
		http.Error(w, fmt.Sprintf("list events: %v", err), http.StatusInternalServerError)
		return
	}

	// Format events as plain text for the LLM prompt.
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
			Role: "system",
			Content: "You are a helpful executive assistant. " +
				"Give the user a concise morning briefing of their upcoming calendar events. " +
				"Be friendly but brief.",
		},
		{
			Role:    "user",
			Content: "Here are my upcoming calendar events:\n\n" + sb.String(),
		},
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("llm chat: %v", err), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"summary": reply})
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

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writeJSON: %v", err)
	}
}
