package server

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/darrint/officeagent/internal/config"
	"github.com/darrint/officeagent/internal/graph"
	"github.com/darrint/officeagent/internal/llm"
)

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
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("GET /login", s.handleLogin)
	s.mux.HandleFunc("GET /login/callback", s.handleLoginCallback)
	s.mux.HandleFunc("GET /login/status", s.handleLoginStatus)
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

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writeJSON: %v", err)
	}
}
