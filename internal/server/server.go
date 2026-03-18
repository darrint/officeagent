package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/darrint/officeagent/internal/activitylog"
	"github.com/darrint/officeagent/internal/config"
	"github.com/darrint/officeagent/internal/fastmail"
	github "github.com/darrint/officeagent/internal/github"
	"github.com/darrint/officeagent/internal/graph"
	"github.com/darrint/officeagent/internal/llm"
	"github.com/darrint/officeagent/internal/ntfy"
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
	TokenExpiry(ctx context.Context) (time.Time, bool)
	ClearToken() error
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

// githubService is the subset of github.Client used by the server.
type githubService interface {
	ListRecentPRs(ctx context.Context, since time.Time, orgs []string, username string) ([]github.PullRequest, error)
}

// fastmailService is the subset of fastmail.Client used by the server.
type fastmailService interface {
	ListMessages(ctx context.Context, top int) ([]fastmail.Message, error)
}

// fastmailMoverService extends fastmailService with archive capabilities.
type fastmailMoverService interface {
	fastmailService
	GetOrCreateMailbox(ctx context.Context, name string) (string, error)
	MoveMessages(ctx context.Context, messageIDs []string, targetMailboxID string) error
}

// fastmailReadOnlyChecker is optionally implemented by the Fastmail client to
// report whether the API token has write access.
type fastmailReadOnlyChecker interface {
	IsReadOnly(ctx context.Context) (bool, error)
}

// graphMoverService extends graphService with archive capabilities.
type graphMoverService interface {
	graphService
	GetOrCreateFolder(ctx context.Context, name string) (string, error)
	MoveMessages(ctx context.Context, messageIDs []string, folderID string) error
}

// classifyMsg is a compact message descriptor sent to the LLM for classification.
type classifyMsg struct {
	ID      string
	From    string
	Subject string
	Preview string
}

// lowPrioMsg is a message identified as low-priority during the assessment phase.
// It carries enough metadata to display in the UI without a round-trip.
type lowPrioMsg struct {
	ID          string    `json:"id"`
	Source      string    `json:"source"`       // "graph" or "fastmail"
	From        string    `json:"from"`
	Subject     string    `json:"subject"`
	ReceivedAt  time.Time `json:"received_at"`
}

// archiveResult is returned as JSON from POST /archive-lowprio.
type archiveResult struct {
	FastmailMoved int    `json:"fastmail_moved"`
	GraphMoved    int    `json:"graph_moved"`
	FastmailError string `json:"fastmail_error,omitempty"`
	GraphError    string `json:"graph_error,omitempty"`
}

// Default system prompts. Used when no custom prompt is stored.
const (
	defaultOverallPrompt   = ""
	defaultEmailPrompt     = "You are a helpful executive assistant. Give the user a concise summary of their recent inbox. Highlight anything urgent or requiring action. Be friendly but brief."
	defaultCalendarPrompt  = "You are a helpful executive assistant. Give the user a concise morning briefing of their upcoming calendar events. Be friendly but brief."
	defaultGitHubPrompt    = "You are a helpful engineering assistant. Give the user a concise summary of recent GitHub pull request activity across their team. Start with the overall picture: what is being worked on, what shipped, what is under review. Then highlight anything that specifically needs the user's attention — review requests, mentions, or their own open PRs awaiting feedback. Be friendly but brief."
	defaultFastmailPrompt  = "You are a helpful personal assistant. Give the user a concise summary of their recent personal inbox. Highlight anything that needs attention or action. Be friendly but brief."
)

// buildSystemPrompt assembles the final system prompt sent to the LLM.
// If overall is non-empty it is prepended to specific, separated by a blank
// line, acting as a global instruction prefix for every section prompt.
func buildSystemPrompt(overall, specific string) string {
	if overall == "" {
		return specific
	}
	return overall + "\n\n" + specific
}

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

// progressEvent is a named step emitted during briefing generation.
type progressEvent struct {
	Step    string // e.g. "email:fetch", "email:llm", "done", "error"
	Message string // human-readable status line
}

// progressBus fans out progress events to all connected SSE clients.
// It also remembers the last terminal event ("done" or "error") so that
// clients which connect after generation finishes receive it immediately.
type progressBus struct {
	mu       sync.Mutex
	clients  map[chan progressEvent]struct{}
	lastDone *progressEvent // non-nil once a terminal event has been published
}

func newProgressBus() *progressBus {
	return &progressBus{clients: make(map[chan progressEvent]struct{})}
}

// subscribe registers a new SSE client and returns its channel.
// If a terminal event was already published, it is pre-queued so the client
// receives it without waiting.
func (b *progressBus) subscribe() chan progressEvent {
	ch := make(chan progressEvent, 16)
	b.mu.Lock()
	b.clients[ch] = struct{}{}
	if b.lastDone != nil {
		ch <- *b.lastDone
	}
	b.mu.Unlock()
	return ch
}

// unsubscribe removes a client channel.
func (b *progressBus) unsubscribe(ch chan progressEvent) {
	b.mu.Lock()
	delete(b.clients, ch)
	b.mu.Unlock()
}

// publish sends an event to all current subscribers (non-blocking per client).
// Terminal events ("done" or "error") are also cached so late subscribers get them.
func (b *progressBus) publish(ev progressEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ev.Step == "done" || ev.Step == "error" {
		b.lastDone = &ev
	}
	for ch := range b.clients {
		select {
		case ch <- ev:
		default: // drop if client is too slow
		}
	}
}

// reset clears the cached terminal event. Called when a new generation starts
// so stale "done" events are not sent to clients connecting mid-generation.
func (b *progressBus) reset() {
	b.mu.Lock()
	b.lastDone = nil
	b.mu.Unlock()
}

// Server is the officeagent HTTP server.
type Server struct {
	cfg      *config.Config
	mux      *http.ServeMux
	auth     authService
	client   graphService
	llm      llmService
	ghClient githubService
	fmClient fastmailService
	store    *store.Store
	alog     *activitylog.Logger // may be nil; writes are no-ops via NewDiscard

	clientMu      sync.RWMutex
	pendingMu     sync.Mutex
	pendingLogins map[string]pendingLogin // state -> pending

	progress *progressBus // SSE fan-out for briefing generation progress
}

// serverOption is a functional option for New.
type serverOption func(*Server)

// WithActivityLog sets the activity logger on the server. If not called the
// server uses activitylog.NewDiscard() so all UI request logging is a no-op.
func WithActivityLog(l *activitylog.Logger) serverOption {
	return func(s *Server) { s.alog = l }
}

// getLLM returns the current LLM client, safe for concurrent use.
func (s *Server) getLLM() llmService {
	s.clientMu.RLock()
	defer s.clientMu.RUnlock()
	return s.llm
}

// getGHClient returns the current GitHub client, safe for concurrent use.
func (s *Server) getGHClient() githubService {
	s.clientMu.RLock()
	defer s.clientMu.RUnlock()
	return s.ghClient
}

// getFMClient returns the current Fastmail client, safe for concurrent use.
func (s *Server) getFMClient() fastmailService {
	s.clientMu.RLock()
	defer s.clientMu.RUnlock()
	return s.fmClient
}

// effectiveGitHubToken returns the GitHub token to use: store value takes
// precedence over the env-var value in cfg so the Settings page is the
// authoritative source.
func (s *Server) effectiveGitHubToken() string {
	if s.store != nil {
		if v, err := s.store.Get("setting.github_token"); err == nil && v != "" {
			return v
		}
	}
	return s.cfg.GitHubToken
}

// effectiveAzureClientID returns the Azure client ID from the store if set,
// otherwise falls back to the value loaded from the env var at startup.
func (s *Server) effectiveAzureClientID() string {
	if s.store != nil {
		if v, err := s.store.Get("setting.azure_client_id"); err == nil && v != "" {
			return v
		}
	}
	return s.cfg.AzureClientID
}

// effectiveAzureTenantID returns the Azure tenant ID from the store if set,
// otherwise falls back to the value loaded from the env var at startup.
func (s *Server) effectiveAzureTenantID() string {
	if s.store != nil {
		if v, err := s.store.Get("setting.azure_tenant_id"); err == nil && v != "" {
			return v
		}
	}
	return s.cfg.AzureTenantID
}

// effectiveFastmailToken returns the Fastmail API token from the store.
func (s *Server) effectiveFastmailToken() string {
	if s.store != nil {
		if v, err := s.store.Get("setting.fastmail_token"); err == nil && v != "" {
			return v
		}
	}
	return ""
}

// reinitClients rebuilds the LLM, GitHub, and Fastmail clients using current
// effective tokens. Called after tokens are updated via the Settings page so
// that new API calls use the updated credentials without requiring a server
// restart.
func (s *Server) reinitClients() {
	ghTok := s.effectiveGitHubToken()
	fmTok := s.effectiveFastmailToken()
	s.clientMu.Lock()
	defer s.clientMu.Unlock()
	if ghTok != "" {
		s.llm = llm.NewClient(ghTok, s.cfg.LLMModel)
		s.ghClient = github.NewClient(ghTok)
	} else {
		s.llm = nil
		s.ghClient = nil
	}
	if fmTok != "" {
		s.fmClient = fastmail.NewClient(fmTok)
	} else {
		s.fmClient = nil
	}
}

// New creates a new Server with routes registered.
func New(cfg *config.Config, auth *graph.Auth, client *graph.Client, llmClient *llm.Client, ghClient *github.Client, fmClient *fastmail.Client, st *store.Store, opts ...serverOption) *Server {
	s := &Server{
		cfg:           cfg,
		mux:           http.NewServeMux(),
		auth:          auth,
		client:        client,
		store:         st,
		alog:          activitylog.NewDiscard(),
		pendingLogins: make(map[string]pendingLogin),
		progress:      newProgressBus(),
	}
	// Apply functional options (e.g. WithActivityLog).
	for _, opt := range opts {
		opt(s)
	}
	// Assign llmClient only when non-nil to preserve nil interface semantics.
	// A typed nil (*llm.Client)(nil) assigned to an interface field would make
	// the field non-nil, causing spurious "LLM not configured" paths to panic.
	if llmClient != nil {
		s.llm = llmClient
	}
	// Same pattern for ghClient and fmClient.
	if ghClient != nil {
		s.ghClient = ghClient
	}
	if fmClient != nil {
		s.fmClient = fmClient
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /", s.handleSummaryPage)
	s.mux.HandleFunc("POST /generate", s.handleGenerate)
	s.mux.HandleFunc("GET /generating", s.handleGeneratingPage)
	s.mux.HandleFunc("GET /generate/progress", s.handleGenerateProgress)
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("GET /connect", s.handleConnect)
	s.mux.HandleFunc("POST /disconnect", s.handleDisconnect)
	s.mux.HandleFunc("GET /doctor", s.handleDoctor)
	s.mux.HandleFunc("GET /login", s.handleLogin)
	s.mux.HandleFunc("GET /login/callback", s.handleLoginCallback)
	s.mux.HandleFunc("GET /login/status", s.handleLoginStatus)
	s.mux.HandleFunc("GET /settings", s.handleSettingsGet)
	s.mux.HandleFunc("POST /settings", s.handleSettingsPost)
	s.mux.HandleFunc("POST /feedback", s.handleFeedback)
	s.mux.HandleFunc("POST /archive-lowprio", s.handleArchiveLowPrio)
	s.mux.HandleFunc("POST /send-report", s.handleSendReport)
	s.mux.HandleFunc("GET /api/mail", s.handleMail)
	s.mux.HandleFunc("GET /api/calendar", s.handleCalendar)
	s.mux.HandleFunc("GET /api/llm/ping", s.handleLLMPing)
	s.mux.HandleFunc("GET /reports", s.handleReportsList)
	s.mux.HandleFunc("GET /reports/{id}", s.handleReportView)
}

// Run starts the HTTP server and blocks until it returns an error.
func (s *Server) Run() error {
	log.Printf("officeagent listening on %s", s.cfg.Addr)
	return http.ListenAndServe(s.cfg.Addr, s.uiLoggingMiddleware(s.mux))
}

// uiLoggingMiddleware wraps a handler and writes one activity log record per
// inbound UI request (subsystem "ui") with method, path, status, and latency.
func (s *Server) uiLoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lrw := &loggingResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(lrw, r)
		s.alog.Write(activitylog.Record{
			Timestamp:  start.UTC(),
			Direction:  "req",
			Subsystem:  "ui",
			Method:     r.Method,
			URL:        r.URL.RequestURI(),
			StatusCode: lrw.statusCode,
			LatencyMS:  time.Since(start).Milliseconds(),
		})
	})
}

// loggingResponseWriter wraps http.ResponseWriter to capture the written status code.
type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (lrw *loggingResponseWriter) WriteHeader(code int) {
	lrw.statusCode = code
	lrw.ResponseWriter.WriteHeader(code)
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
.gen-bar{display:flex;align-items:center;gap:1rem;margin-bottom:1.5rem;padding:.6rem 1rem;background:#fff;border-radius:8px;box-shadow:0 1px 4px rgba(0,0,0,.06)}
.gen-bar span{flex:1;font-size:.82rem;color:#888}
.gen-bar button{background:#0078d4;color:#fff;border:none;border-radius:6px;padding:.4rem 1rem;font-size:.85rem;font-weight:600;cursor:pointer}
.gen-bar button:hover{background:#006cbd}
.gen-bar button:disabled{background:#99c0e8;cursor:not-allowed}
.empty-state{text-align:center;padding:4rem 2rem}
.empty-state p{color:#888;margin-bottom:1.5rem}
.empty-state button{background:#0078d4;color:#fff;border:none;border-radius:8px;padding:.75rem 2rem;font-size:1rem;font-weight:600;cursor:pointer}
.empty-state button:hover{background:#006cbd}
.empty-state button:disabled{background:#99c0e8;cursor:not-allowed}
@keyframes spin{to{transform:rotate(360deg)}}
.spinner{display:inline-block;width:.85em;height:.85em;border:2px solid rgba(255,255,255,.4);border-top-color:#fff;border-radius:50%;animation:spin .6s linear infinite;vertical-align:middle;margin-right:.35em}
.lowprio-list{list-style:none;margin:.5rem 0 0;padding:0}
.lowprio-list li{display:flex;gap:.75rem;padding:.35rem 0;border-bottom:1px solid #f0f0f0;font-size:.82rem;color:#555;align-items:baseline}
.lowprio-list li:last-child{border-bottom:none}
.lowprio-list .lp-from{font-weight:600;color:#444;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;max-width:180px;flex-shrink:0}
.lowprio-list .lp-subj{flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.lowprio-list .lp-date{white-space:nowrap;color:#aaa;font-size:.78rem;flex-shrink:0}
.lowprio-list .lp-src{font-size:.72rem;font-weight:700;border-radius:3px;padding:.1rem .35rem;flex-shrink:0;white-space:nowrap}
.lowprio-list .lp-src-graph{background:#dce8f5;color:#0550ae}
.lowprio-list .lp-src-fastmail{background:#e8f5e9;color:#1a7f37}
.lowprio-panel{padding:.75rem 1rem;background:#fafafa;border:1px solid #ececec;border-radius:8px;margin-top:.5rem}
.lowprio-actions{display:flex;align-items:center;gap:1rem;margin-top:.75rem}
.lowprio-actions button{background:#555;color:#fff;border:none;border-radius:6px;padding:.35rem .9rem;font-size:.82rem;font-weight:600;cursor:pointer}
.lowprio-actions button:hover{background:#333}
.lowprio-actions button:disabled{background:#aaa;cursor:not-allowed}
.lowprio-actions .lp-result{font-size:.8rem;color:#555}
</style>
<script>
function startGenerate() {
  fetch('/generate', {method:'POST', redirect:'manual'})
    .then(function() { window.location.href = '/generating'; })
    .catch(function() { window.location.href = '/generating'; });
}
function archiveLowPrio() {
  var btn = document.getElementById('archive-btn');
  var out = document.getElementById('lp-result');
  btn.disabled = true;
  btn.innerHTML = '<span class="spinner" style="border-color:rgba(255,255,255,.3);border-top-color:#fff"></span>Moving\u2026';
  fetch('/archive-lowprio', {method:'POST'})
    .then(function(r){ return r.json(); })
    .then(function(d){
      btn.disabled = false;
      btn.innerHTML = 'Move to Low-Priority Folder';
      var parts = [];
      if (d.fastmail_moved > 0) parts.push('Fastmail: ' + d.fastmail_moved + ' moved');
      if (d.graph_moved > 0) parts.push('Office 365: ' + d.graph_moved + ' moved');
      if (d.fastmail_error) parts.push('Fastmail error: ' + d.fastmail_error);
      if (d.graph_error) parts.push('Office 365 error: ' + d.graph_error);
      if (parts.length === 0) parts.push('No low-priority mail to move.');
      out.textContent = parts.join(' \u00b7 ');
    })
    .catch(function(e){
      btn.disabled = false;
      btn.innerHTML = 'Move to Low-Priority Folder';
      out.textContent = 'Error: ' + e;
    });
}
function sendReport() {
  var btn = document.getElementById('send-report-btn');
  var out = document.getElementById('send-result');
  btn.disabled = true;
  btn.innerHTML = '<span class="spinner"></span>Sending\u2026';
  fetch('/send-report', {method:'POST'})
    .then(function(r){
      if (!r.ok) return r.text().then(function(t){ throw new Error(t); });
      btn.disabled = false;
      btn.innerHTML = 'Send Now';
      out.textContent = 'Report sent via ntfy.';
    })
    .catch(function(e){
      btn.disabled = false;
      btn.innerHTML = 'Send Now';
      out.textContent = 'Send failed: ' + e;
    });
}
</script>
</head>
<body>
<div class="wrap">
  <header>
    <h1>officeagent</h1>
    <span>morning briefing</span>
    <a href="/connect">Connect</a>
    <a href="/settings">Settings</a>
    <a href="/reports">History</a>
  </header>
  {{if .FatalError}}
  <div class="error">{{.FatalError}}</div>
  {{else if .GeneratedAt}}
  <div class="gen-bar">
    <span>Generated {{.GeneratedAt}}</span>
    <button type="button" onclick="startGenerate()">Regenerate</button>
    <button type="button" id="send-report-btn" onclick="sendReport()" style="background:#107c10">Send Now</button>
  </div>
  <div id="send-result" style="font-size:.82rem;color:#555;margin-bottom:.75rem;padding:0 1rem"></div>
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
  {{if or .GitHub.Error .GitHub.HTML}}
  <div class="section">
    <div class="section-title">GitHub PRs</div>
    {{if .GitHub.Error}}<div class="error">{{.GitHub.Error}}</div>
    {{else}}
    <div class="card">{{.GitHub.HTML}}</div>
    <form method="POST" action="/feedback" class="feedback">
      <input type="hidden" name="section" value="github">
      <button type="submit" name="rating" value="good" title="Helpful">👍</button>
      <button type="submit" name="rating" value="bad" title="Needs improvement">👎</button>
      <input type="text" name="note" placeholder="What should be different? (optional)" maxlength="300">
    </form>
    {{end}}
  </div>
  {{end}}
  {{if or .Fastmail.Error .Fastmail.HTML}}
  <div class="section">
    <div class="section-title">Personal Email (Fastmail)</div>
    {{if .Fastmail.Error}}<div class="error">{{.Fastmail.Error}}</div>
    {{else}}
    <div class="card">{{.Fastmail.HTML}}</div>
    <form method="POST" action="/feedback" class="feedback">
      <input type="hidden" name="section" value="fastmail">
      <button type="submit" name="rating" value="good" title="Helpful">👍</button>
      <button type="submit" name="rating" value="bad" title="Needs improvement">👎</button>
      <input type="text" name="note" placeholder="What should be different? (optional)" maxlength="300">
    </form>
    {{end}}
  </div>
  {{end}}
  {{if .LowPrioMsgs}}
  <details>
    <summary>Low-priority mail ({{len .LowPrioMsgs}} message{{if gt (len .LowPrioMsgs) 1}}s{{end}} identified)</summary>
    <div class="lowprio-panel">
      <ul class="lowprio-list">
        {{range .LowPrioMsgs}}
        <li>
          {{if eq .Source "graph"}}<span class="lp-src lp-src-graph">Work</span>{{else if eq .Source "fastmail"}}<span class="lp-src lp-src-fastmail">Personal</span>{{end}}
          <span class="lp-from">{{.From}}</span>
          <span class="lp-subj">{{.Subject}}</span>
          <span class="lp-date">{{.ReceivedAt.Format "Jan 2 3:04 PM"}}</span>
        </li>
        {{end}}
      </ul>
      <div class="lowprio-actions">
        <button type="button" id="archive-btn" onclick="archiveLowPrio()">Move to Low-Priority Folder</button>
        <span id="lp-result" class="lp-result"></span>
      </div>
    </div>
  </details>
  {{end}}
  <details>
    <summary>Raw JSON</summary>
    <pre>{{.RawJSON}}</pre>
  </details>
  {{else}}
  <div class="empty-state">
    <p>No briefing generated yet.</p>
    <button type="button" onclick="startGenerate()">Generate Briefing</button>
  </div>
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
	Email       sectionData
	Calendar    sectionData
	GitHub      sectionData
	Fastmail    sectionData
	LowPrioMsgs []lowPrioMsg
	RawJSON     string
	GeneratedAt string // empty = no cached report yet
	FatalError  string
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

// getSetting returns a non-prompt setting value from the store, or defaultVal.
func (s *Server) getSetting(key, defaultVal string) string {
	if s.store == nil {
		return defaultVal
	}
	val, err := s.store.Get("setting." + key)
	if err != nil {
		log.Printf("store get setting.%s: %v", key, err)
		return defaultVal
	}
	if val == "" {
		return defaultVal
	}
	return val
}

// lastWorkDaySince returns midnight of the most recent work day before now.
// If today is Monday it returns Friday; otherwise it returns yesterday,
// skipping Saturday and Sunday.
func lastWorkDaySince(now time.Time) time.Time {
	day := now.Truncate(24 * time.Hour)
	switch now.Weekday() {
	case time.Monday:
		day = day.AddDate(0, 0, -3) // Friday
	default:
		day = day.AddDate(0, 0, -1)
		// Keep stepping back if we land on a weekend.
		for day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
			day = day.AddDate(0, 0, -1)
		}
	}
	return day
}

// githubSince returns the time.Time to use as the "updated since" cutoff for
// GitHub PR queries. Reads setting.github.lookback_days from the store;
// "0" (or unset) means auto (last work day).
func (s *Server) githubSince() time.Time {
	raw := s.getSetting("github.lookback_days", "0")
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return lastWorkDaySince(time.Now())
	}
	return time.Now().AddDate(0, 0, -n)
}

// githubOrgs returns the list of GitHub org filters from settings.
// Returns nil (meaning "all orgs") if the setting is empty.
func (s *Server) githubOrgs() []string {
	raw := strings.TrimSpace(s.getSetting("github.orgs", ""))
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	orgs := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			orgs = append(orgs, p)
		}
	}
	return orgs
}

// githubUsername returns the GitHub username from settings, used to include
// personal repo PRs alongside org-scoped results.
func (s *Server) githubUsername() string {
	return strings.TrimSpace(s.getSetting("github.username", ""))
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
input[type=text],input[type=password]{width:100%;padding:.75rem;font-family:system-ui,sans-serif;font-size:.9rem;border:1px solid #ddd;border-radius:6px;color:#1a1a1a}
input[type=text]:focus,input[type=password]:focus{outline:none;border-color:#0078d4;box-shadow:0 0 0 2px rgba(0,120,212,.15)}
.token-set{display:inline-block;background:#dff6dd;color:#107c10;border-radius:4px;padding:.15rem .5rem;font-size:.78rem;font-weight:600;margin-left:.5rem}
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
      <label for="overall_prompt">Overall prompt prefix</label>
      <textarea id="overall_prompt" name="overall_prompt" rows="4">{{.OverallPrompt}}</textarea>
      <p class="hint">This text is prepended to every section prompt. Use it to set tone, persona, or standing instructions that apply to all summaries.</p>
    </div>
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
    <div class="card">
      <label for="github_prompt">GitHub PR summary prompt</label>
      <textarea id="github_prompt" name="github_prompt" rows="4">{{.GitHubPrompt}}</textarea>
      <p class="hint">This system prompt is sent to the LLM when summarizing your recent GitHub PR activity.</p>
    </div>
    <div class="card">
      <label for="fastmail_prompt">Fastmail summary prompt</label>
      <textarea id="fastmail_prompt" name="fastmail_prompt" rows="4">{{.FastmailPrompt}}</textarea>
      <p class="hint">This system prompt is sent to the LLM when summarizing your personal Fastmail inbox.</p>
    </div>
    <div class="card">
      <label for="github_lookback_days">GitHub lookback days</label>
      <textarea id="github_lookback_days" name="github_lookback_days" rows="1">{{.GitHubLookbackDays}}</textarea>
      <p class="hint">Number of days of PR activity to include. Set to 0 for auto (since last work day).</p>
    </div>
    <div class="card">
      <label for="github_orgs">GitHub organizations (optional)</label>
      <textarea id="github_orgs" name="github_orgs" rows="2">{{.GitHubOrgs}}</textarea>
      <p class="hint">Comma-separated list of GitHub org names to filter PRs by. Leave blank to search all accessible repos.</p>
    </div>
    <div class="card">
      <label for="github_username">GitHub username (optional)</label>
      <input type="text" id="github_username" name="github_username" value="{{.GitHubUsername}}" placeholder="your-github-login">
      <p class="hint">Your GitHub username. When org filters are set, personal repo PRs are only included if you enter your username here.</p>
    </div>
    <div class="card">
      <label for="github_token">GitHub token{{if .GitHubTokenSet}}<span class="token-set">&#10003; Token is set</span>{{end}}</label>
      <input type="password" id="github_token" name="github_token" autocomplete="new-password" placeholder="Leave blank to keep existing token">
      <p class="hint">GitHub OAuth token with <code>copilot</code> scope, used for LLM and GitHub PR features. Run <code>gh auth login --scopes copilot</code> then <code>gh auth token</code> to obtain one. Never echoed back to the browser.</p>
    </div>
    <div class="card">
      <label for="fastmail_token">Fastmail API token{{if .FastmailTokenSet}}<span class="token-set">&#10003; Token is set</span>{{end}}</label>
      <input type="password" id="fastmail_token" name="fastmail_token" autocomplete="new-password" placeholder="Leave blank to keep existing token">
      <p class="hint">Fastmail API token for personal inbox summaries and mail moving. Generate one at <code>app.fastmail.com/settings/security/tokens</code> — select <strong>Mail (read-write)</strong> access (not read-only). Never echoed back to the browser.</p>
    </div>
    <div class="card">
      <label for="azure_client_id">Azure application (client) ID</label>
      <input type="text" id="azure_client_id" name="azure_client_id" value="{{.AzureClientID}}" placeholder="xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx">
      <p class="hint">Azure AD app client ID used for Microsoft Graph OAuth. Register the app in Azure Portal under "Mobile and desktop applications" with redirect URI <code>http://localhost:8080/login/callback</code>.</p>
    </div>
    <div class="card">
      <label for="azure_tenant_id">Azure tenant ID</label>
      <input type="text" id="azure_tenant_id" name="azure_tenant_id" value="{{.AzureTenantID}}" placeholder="common">
      <p class="hint">Azure AD tenant ID. Use <code>common</code> for personal Microsoft accounts / multi-tenant, or paste your organisation's tenant GUID or domain (e.g. <code>contoso.onmicrosoft.com</code>).</p>
    </div>
    <div class="card">
      <label for="fastmail_lowprio_folder">Fastmail low-priority folder name</label>
      <input type="text" id="fastmail_lowprio_folder" name="fastmail_lowprio_folder" value="{{.FastmailLowPrioFolder}}" placeholder="Low Priority">
      <p class="hint">Fastmail mailbox to move low-priority mail into when "Move Low-Priority Mail" is used.</p>
    </div>
    <div class="card">
      <label for="graph_lowprio_folder">Office 365 low-priority folder name</label>
      <input type="text" id="graph_lowprio_folder" name="graph_lowprio_folder" value="{{.GraphLowPrioFolder}}" placeholder="Low Priority">
      <p class="hint">Office 365 mail folder to move low-priority mail into when "Move Low-Priority Mail" is used.</p>
    </div>
    <div class="card">
      <label for="ntfy_topic">ntfy.sh topic (push notifications)</label>
      <input type="text" id="ntfy_topic" name="ntfy_topic" value="{{.NtfyTopic}}" placeholder="your-secret-topic-name">
      <p class="hint">Secret ntfy.sh topic name for 7 AM daily briefing push notifications. Leave blank to disable. Create a topic at <code>ntfy.sh</code> — keep it secret.</p>
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
	OverallPrompt          string
	EmailPrompt            string
	CalendarPrompt         string
	GitHubPrompt           string
	FastmailPrompt         string
	GitHubLookbackDays     string
	GitHubOrgs             string
	GitHubUsername         string
	GitHubTokenSet         bool   // true if a GitHub token is stored (never echo the value)
	FastmailTokenSet       bool   // true if a Fastmail token is stored (never echo the value)
	AzureClientID          string // not a secret — can be shown in the UI
	AzureTenantID          string // not a secret — can be shown in the UI
	FastmailLowPrioFolder  string
	GraphLowPrioFolder     string
	NtfyTopic              string // stored as setting.ntfy_topic; shown as password input
	Saved                  bool
}

func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	if !s.auth.IsAuthenticated(r.Context()) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	data := settingsData{
		OverallPrompt:         s.getPrompt("overall", defaultOverallPrompt),
		EmailPrompt:           s.getPrompt("email", defaultEmailPrompt),
		CalendarPrompt:        s.getPrompt("calendar", defaultCalendarPrompt),
		GitHubPrompt:          s.getPrompt("github", defaultGitHubPrompt),
		FastmailPrompt:        s.getPrompt("fastmail", defaultFastmailPrompt),
		GitHubLookbackDays:    s.getSetting("github.lookback_days", "0"),
		GitHubOrgs:            s.getSetting("github.orgs", ""),
		GitHubUsername:        s.getSetting("github.username", ""),
		GitHubTokenSet:        s.effectiveGitHubToken() != "",
		FastmailTokenSet:      s.effectiveFastmailToken() != "",
		AzureClientID:         s.effectiveAzureClientID(),
		AzureTenantID:         s.effectiveAzureTenantID(),
		FastmailLowPrioFolder: s.getSetting("fastmail_lowprio_folder", "Low Priority"),
		GraphLowPrioFolder:    s.getSetting("graph_lowprio_folder", "Low Priority"),
		NtfyTopic:             s.getSetting("ntfy_topic", ""),
		Saved:                 r.URL.Query().Get("saved") == "1",
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
	overallPrompt := strings.TrimSpace(r.FormValue("overall_prompt"))
	githubPrompt := strings.TrimSpace(r.FormValue("github_prompt"))
	fastmailPrompt := strings.TrimSpace(r.FormValue("fastmail_prompt"))
	githubLookback := strings.TrimSpace(r.FormValue("github_lookback_days"))
	githubOrgs := strings.TrimSpace(r.FormValue("github_orgs"))
	githubUsername := strings.TrimSpace(r.FormValue("github_username"))
	githubToken := strings.TrimSpace(r.FormValue("github_token"))
	fastmailToken := strings.TrimSpace(r.FormValue("fastmail_token"))
	azureClientID := strings.TrimSpace(r.FormValue("azure_client_id"))
	azureTenantID := strings.TrimSpace(r.FormValue("azure_tenant_id"))
	fastmailLowPrioFolder := strings.TrimSpace(r.FormValue("fastmail_lowprio_folder"))
	graphLowPrioFolder := strings.TrimSpace(r.FormValue("graph_lowprio_folder"))
	ntfyTopic := strings.TrimSpace(r.FormValue("ntfy_topic"))

	if s.store != nil {
		if err := s.store.Set("prompt.overall", overallPrompt); err != nil {
			log.Printf("store set prompt.overall: %v", err)
		}
		if err := s.store.Set("prompt.email", emailPrompt); err != nil {
			log.Printf("store set prompt.email: %v", err)
		}
		if err := s.store.Set("prompt.calendar", calPrompt); err != nil {
			log.Printf("store set prompt.calendar: %v", err)
		}
		if err := s.store.Set("prompt.github", githubPrompt); err != nil {
			log.Printf("store set prompt.github: %v", err)
		}
		if err := s.store.Set("prompt.fastmail", fastmailPrompt); err != nil {
			log.Printf("store set prompt.fastmail: %v", err)
		}
		if err := s.store.Set("setting.github.lookback_days", githubLookback); err != nil {
			log.Printf("store set setting.github.lookback_days: %v", err)
		}
		if err := s.store.Set("setting.github.orgs", githubOrgs); err != nil {
			log.Printf("store set setting.github.orgs: %v", err)
		}
		if err := s.store.Set("setting.github.username", githubUsername); err != nil {
			log.Printf("store set setting.github.username: %v", err)
		}
		// Only update the GitHub token if a non-empty value was submitted.
		// An empty submission means "keep existing token".
		if githubToken != "" {
			if err := s.store.Set("setting.github_token", githubToken); err != nil {
				log.Printf("store set setting.github_token: %v", err)
			}
			// Rebuild LLM / GitHub clients immediately with the new token.
			s.reinitClients()
		}
		// Same pattern for Fastmail token.
		if fastmailToken != "" {
			if err := s.store.Set("setting.fastmail_token", fastmailToken); err != nil {
				log.Printf("store set setting.fastmail_token: %v", err)
			}
			s.reinitClients()
		}
		if azureClientID != "" {
			if err := s.store.Set("setting.azure_client_id", azureClientID); err != nil {
				log.Printf("store set setting.azure_client_id: %v", err)
			}
		}
		if azureTenantID != "" {
			if err := s.store.Set("setting.azure_tenant_id", azureTenantID); err != nil {
				log.Printf("store set setting.azure_tenant_id: %v", err)
			}
		}
		if fastmailLowPrioFolder != "" {
			if err := s.store.Set("setting.fastmail_lowprio_folder", fastmailLowPrioFolder); err != nil {
				log.Printf("store set setting.fastmail_lowprio_folder: %v", err)
			}
		}
		if graphLowPrioFolder != "" {
			if err := s.store.Set("setting.graph_lowprio_folder", graphLowPrioFolder); err != nil {
				log.Printf("store set setting.graph_lowprio_folder: %v", err)
			}
		}
		// ntfy_topic is always saved (including empty string to clear it).
		if err := s.store.Set("setting.ntfy_topic", ntfyTopic); err != nil {
			log.Printf("store set setting.ntfy_topic: %v", err)
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

	if section != "email" && section != "calendar" && section != "github" && section != "fastmail" {
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

// parseLLMIDs extracts a JSON array of string IDs from an LLM reply.
// It strips markdown code fences before parsing.
func parseLLMIDs(reply string) []string {
	s := strings.TrimSpace(reply)
	// Strip markdown code fences.
	if idx := strings.Index(s, "```"); idx >= 0 {
		s = s[idx+3:]
		if nl := strings.Index(s, "\n"); nl >= 0 {
			s = s[nl+1:]
		}
		if end := strings.LastIndex(s, "```"); end >= 0 {
			s = s[:end]
		}
	}
	s = strings.TrimSpace(s)
	// Find JSON array bounds.
	start := strings.Index(s, "[")
	end := strings.LastIndex(s, "]")
	if start < 0 || end <= start {
		return nil
	}
	var ids []string
	if err := json.Unmarshal([]byte(s[start:end+1]), &ids); err != nil {
		log.Printf("parseLLMIDs: unmarshal error: %v (input: %q)", err, s[start:end+1])
		return nil
	}
	return ids
}

// filterToKnownIDs returns only the IDs from proposed that appear in known.
// This prevents the LLM from returning hallucinated IDs.
func filterToKnownIDs(proposed []string, known []classifyMsg) []string {
	set := make(map[string]struct{}, len(known))
	for _, m := range known {
		set[m.ID] = struct{}{}
	}
	var out []string
	for _, id := range proposed {
		if _, ok := set[id]; ok {
			out = append(out, id)
		}
	}
	return out
}

// classifyLowPriority asks the LLM to identify low-priority messages from msgs.
// Returns the IDs the LLM identified as low priority (unvalidated — caller must
// call filterToKnownIDs before using them).
func classifyLowPriority(ctx context.Context, msgs []classifyMsg, llmC llmService) ([]string, error) {
	if len(msgs) == 0 {
		return nil, nil
	}
	var sb strings.Builder
	for _, m := range msgs {
		fmt.Fprintf(&sb, "ID: %s\nFrom: %s\nSubject: %s\nPreview: %s\n\n", m.ID, m.From, m.Subject, m.Preview)
	}
	systemPrompt := "You are an email assistant. Identify which of the following emails are low priority: newsletters, marketing, automated notifications, promotional offers, social media digests, and other non-actionable bulk messages. Return ONLY a JSON array of IDs for the low-priority messages. No explanation, no markdown, just the JSON array. If none are low priority return []."
	reply, err := llmC.Chat(ctx, []llm.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: sb.String()},
	})
	if err != nil {
		return nil, fmt.Errorf("LLM classify: %w", err)
	}
	return parseLLMIDs(reply), nil
}

// cachedLowPrioIDs returns the IDs of low-priority messages for the given
// source ("graph" or "fastmail") from the last cached report. Returns nil (not
// an empty slice) when no cached report exists, signalling the caller to fall
// back to live classification. Returns a non-empty errStr on store errors.
func (s *Server) cachedLowPrioIDs(_ context.Context, source string) ([]string, string) {
	rep, err := s.loadLastReport()
	if err != nil {
		return nil, fmt.Sprintf("load last report: %v", err)
	}
	if rep == nil || len(rep.LowPrioMsgs) == 0 {
		return nil, ""
	}
	var ids []string
	for _, m := range rep.LowPrioMsgs {
		if m.Source == source {
			ids = append(ids, m.ID)
		}
	}
	return ids, "" // may be empty slice (none for this source), which is valid
}

// archiveFastmailLowPrio moves low-priority Fastmail messages using IDs cached
// during the last GenerateBriefing run. Falls back to live LLM classification
// only when no cached IDs are available.
// Returns count moved and error string (empty on success).
func (s *Server) archiveFastmailLowPrio(ctx context.Context, llmC llmService) (int, string) {
	fmC, ok := s.getFMClient().(fastmailMoverService)
	if !ok {
		return 0, "" // Fastmail not configured or client doesn't support moving
	}
	folderName := s.getSetting("fastmail_lowprio_folder", "Low Priority")

	// Use cached IDs from the last briefing generation if available.
	ids, errStr := s.cachedLowPrioIDs(ctx, "fastmail")
	if errStr != "" {
		return 0, errStr
	}

	// Fallback: re-classify live if no cached IDs.
	if ids == nil {
		rawMsgs, err := fmC.ListMessages(ctx, 30)
		if err != nil {
			return 0, fmt.Sprintf("list messages: %v", err)
		}
		msgs := make([]classifyMsg, len(rawMsgs))
		for i, m := range rawMsgs {
			msgs[i] = classifyMsg{ID: m.ID, From: m.From, Subject: m.Subject, Preview: m.BodyPreview}
		}
		proposed, err := classifyLowPriority(ctx, msgs, llmC)
		if err != nil {
			return 0, err.Error()
		}
		ids = filterToKnownIDs(proposed, msgs)
	}

	if len(ids) == 0 {
		return 0, ""
	}

	mailboxID, err := fmC.GetOrCreateMailbox(ctx, folderName)
	if err != nil {
		return 0, fmt.Sprintf("get/create mailbox: %v", err)
	}
	if err := fmC.MoveMessages(ctx, ids, mailboxID); err != nil {
		return 0, fmt.Sprintf("move messages: %v", err)
	}
	return len(ids), ""
}

// archiveGraphLowPrio moves low-priority Graph (Office 365) messages using IDs
// cached during the last GenerateBriefing run. Falls back to live LLM
// classification only when no cached IDs are available.
// Returns count moved and error string (empty on success).
func (s *Server) archiveGraphLowPrio(ctx context.Context, llmC llmService) (int, string) {
	gC, ok := s.client.(graphMoverService)
	if !ok {
		return 0, "" // Graph not configured or client doesn't support moving
	}
	folderName := s.getSetting("graph_lowprio_folder", "Low Priority")

	// Use cached IDs from the last briefing generation if available.
	ids, errStr := s.cachedLowPrioIDs(ctx, "graph")
	if errStr != "" {
		return 0, errStr
	}

	// Fallback: re-classify live if no cached IDs.
	if ids == nil {
		rawMsgs, err := gC.ListMessages(ctx, 30)
		if err != nil {
			return 0, fmt.Sprintf("list messages: %v", err)
		}
		msgs := make([]classifyMsg, len(rawMsgs))
		for i, m := range rawMsgs {
			msgs[i] = classifyMsg{ID: m.ID, From: m.From, Subject: m.Subject, Preview: m.BodyPreview}
		}
		proposed, err := classifyLowPriority(ctx, msgs, llmC)
		if err != nil {
			return 0, err.Error()
		}
		ids = filterToKnownIDs(proposed, msgs)
	}

	if len(ids) == 0 {
		return 0, ""
	}

	folderID, err := gC.GetOrCreateFolder(ctx, folderName)
	if err != nil {
		return 0, fmt.Sprintf("get/create folder: %v", err)
	}
	if err := gC.MoveMessages(ctx, ids, folderID); err != nil {
		return 0, fmt.Sprintf("move messages: %v", err)
	}
	return len(ids), ""
}

// handleArchiveLowPrio handles POST /archive-lowprio — classifies inbox messages
// from both Fastmail and Graph and moves low-priority ones to a holding folder.
func (s *Server) handleArchiveLowPrio(w http.ResponseWriter, r *http.Request) {
	if !s.auth.IsAuthenticated(r.Context()) {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}
	llmC := s.getLLM()
	if llmC == nil {
		http.Error(w, "LLM not configured", http.StatusServiceUnavailable)
		return
	}

	ctx := r.Context()
	var result archiveResult

	// Run Fastmail and Graph archives concurrently.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		n, errStr := s.archiveFastmailLowPrio(ctx, llmC)
		result.FastmailMoved = n
		result.FastmailError = errStr
	}()
	go func() {
		defer wg.Done()
		n, errStr := s.archiveGraphLowPrio(ctx, llmC)
		result.GraphMoved = n
		result.GraphError = errStr
	}()
	wg.Wait()

	writeJSON(w, result)
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

	if s.getLLM() == nil {
		servePage(pageData{FatalError: "LLM not configured — set GITHUB_TOKEN"})
		return
	}

	// Render from cache — no API or LLM calls on a plain page load.
	cached, err := s.loadLastReport()
	if err != nil {
		log.Printf("load last report: %v", err)
	}
	if cached == nil {
		// No report yet — show empty state with Generate button.
		servePage(pageData{})
		return
	}

	rawJSON, _ := json.MarshalIndent(map[string]string{
		"email":    cached.EmailRaw,
		"calendar": cached.CalendarRaw,
		"github":   cached.GitHubRaw,
		"fastmail": cached.FastmailRaw,
	}, "", "  ")

	servePage(pageData{
		Email:       sectionDataFromCache(cached.EmailRaw, cached.EmailError),
		Calendar:    sectionDataFromCache(cached.CalendarRaw, cached.CalendarError),
		GitHub:      sectionDataFromCache(cached.GitHubRaw, cached.GitHubError),
		Fastmail:    sectionDataFromCache(cached.FastmailRaw, cached.FastmailError),
		LowPrioMsgs: cached.LowPrioMsgs,
		RawJSON:     string(rawJSON),
		GeneratedAt: cached.GeneratedAt.In(easternLoc).Format("Mon Jan 2 3:04 PM MST"),
	})
}

// sectionDataFromCache reconstructs a sectionData from stored raw text / error.
func sectionDataFromCache(raw, errStr string) sectionData {
	if errStr != "" {
		return sectionData{Error: errStr}
	}
	if raw == "" {
		return sectionData{}
	}
	return sectionData{HTML: renderMarkdown(raw), Raw: raw}
}

// cachedReport is the serialised form of a generated morning briefing stored
// in SQLite so that GET / can render without hitting any external APIs.
type cachedReport struct {
	EmailRaw       string       `json:"email_raw"`
	CalendarRaw    string       `json:"calendar_raw"`
	GitHubRaw      string       `json:"github_raw"`
	FastmailRaw    string       `json:"fastmail_raw,omitempty"`
	EmailError     string       `json:"email_error,omitempty"`
	CalendarError  string       `json:"calendar_error,omitempty"`
	GitHubError    string       `json:"github_error,omitempty"`
	FastmailError  string       `json:"fastmail_error,omitempty"`
	LowPrioMsgs    []lowPrioMsg `json:"low_prio_msgs,omitempty"`
	GeneratedAt    time.Time    `json:"generated_at"`
}

const reportStoreKey = "report.last"

// loadLastReport retrieves the last cached report from the store.
// Returns (nil, nil) when no report has been generated yet.
func (s *Server) loadLastReport() (*cachedReport, error) {
	if s.store == nil {
		return nil, nil
	}
	raw, err := s.store.Get(reportStoreKey)
	if err != nil {
		return nil, fmt.Errorf("store get %s: %w", reportStoreKey, err)
	}
	if raw == "" {
		return nil, nil
	}
	var r cachedReport
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return nil, fmt.Errorf("unmarshal report: %w", err)
	}
	return &r, nil
}

// saveLastReport persists the cached report to the store (kv key + reports table).
func (s *Server) saveLastReport(rep *cachedReport) error {
	if s.store == nil {
		return nil
	}
	b, err := json.Marshal(rep)
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}
	content := string(b)
	// Keep the kv key for fast "latest" lookup.
	if err := s.store.Set(reportStoreKey, content); err != nil {
		return err
	}
	// Also append to the historical reports table.
	if _, err := s.store.SaveReport(rep.GeneratedAt, content); err != nil {
		log.Printf("save report history: %v", err)
	}
	return nil
}

// GenerateBriefing fetches data from all configured sources, asks the LLM for
// summaries of each section, saves the result to the store, and returns it.
// It is safe to call from background goroutines (uses the supplied context).
// Progress events are published to s.progress throughout execution.
func (s *Server) GenerateBriefing(ctx context.Context) (*cachedReport, error) {
	// Clear any cached terminal event from the previous run so clients that
	// connect during this generation don't immediately receive a stale "done".
	s.progress.reset()

	emit := func(step, msg string) {
		s.progress.publish(progressEvent{Step: step, Message: msg})
	}

	llmC := s.getLLM()
	ghC := s.getGHClient()
	if llmC == nil {
		return nil, fmt.Errorf("LLM not configured")
	}

	type emailResult struct {
		section  sectionData
		lowPrios []lowPrioMsg
	}
	type calResult struct{ section sectionData }
	type ghResult struct{ section sectionData }
	type fmResult struct {
		section  sectionData
		lowPrios []lowPrioMsg
	}

	emailCh := make(chan emailResult, 1)
	calCh := make(chan calResult, 1)
	ghCh := make(chan ghResult, 1)
	fmCh := make(chan fmResult, 1)

	go func() {
		emit("email:fetch", "Fetching work email...")
		msgs, err := s.client.ListMessages(ctx, 20)
		if err != nil {
			emit("email:error", fmt.Sprintf("Email fetch failed: %v", err))
			emailCh <- emailResult{section: sectionData{Error: fmt.Sprintf("Failed to fetch email: %v", err)}}
			return
		}
		emit("email:llm", fmt.Sprintf("Summarising %d email(s) with AI...", len(msgs)))

		// Build classifyMsg list from raw messages for low-prio classification.
		classifyMsgs := make([]classifyMsg, len(msgs))
		for i, m := range msgs {
			classifyMsgs[i] = classifyMsg{ID: m.ID, From: m.From, Subject: m.Subject, Preview: m.BodyPreview}
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
		reply, err := llmC.Chat(ctx, []llm.Message{
			{
				Role: "system",
				Content: buildSystemPrompt(
					s.getPrompt("overall", defaultOverallPrompt),
					s.getPrompt("email", defaultEmailPrompt)+s.feedbackContext("email"),
				),
			},
			{Role: "user", Content: "Here are my recent emails:\n\n" + sb.String()},
		})
		if err != nil {
			emit("email:error", fmt.Sprintf("Email AI summary failed: %v", err))
			emailCh <- emailResult{section: sectionData{Error: fmt.Sprintf("LLM error (email): %v", err)}}
			return
		}

		// Classify low-priority messages using the already-fetched list.
		emit("email:classify", "Classifying low-priority work email...")
		proposed, classErr := classifyLowPriority(ctx, classifyMsgs, llmC)
		var graphLowPrios []lowPrioMsg
		if classErr != nil {
			log.Printf("GenerateBriefing: classify graph low-prio: %v", classErr)
		} else {
			ids := filterToKnownIDs(proposed, classifyMsgs)
			idSet := make(map[string]struct{}, len(ids))
			for _, id := range ids {
				idSet[id] = struct{}{}
			}
			for _, m := range msgs {
				if _, ok := idSet[m.ID]; ok {
					graphLowPrios = append(graphLowPrios, lowPrioMsg{
						ID:         m.ID,
						Source:     "graph",
						From:       m.From,
						Subject:    m.Subject,
						ReceivedAt: m.ReceivedAt,
					})
				}
			}
		}

		emit("email:done", "Work email summary ready.")
		emailCh <- emailResult{
			section:  sectionData{HTML: renderMarkdown(reply), Raw: reply},
			lowPrios: graphLowPrios,
		}
	}()

	go func() {
		emit("calendar:fetch", "Fetching calendar events...")
		events, err := s.client.ListEvents(ctx, 20)
		if err != nil {
			emit("calendar:error", fmt.Sprintf("Calendar fetch failed: %v", err))
			calCh <- calResult{sectionData{Error: fmt.Sprintf("Failed to fetch calendar: %v", err)}}
			return
		}
		emit("calendar:llm", fmt.Sprintf("Summarising %d event(s) with AI...", len(events)))
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
		reply, err := llmC.Chat(ctx, []llm.Message{
			{
				Role: "system",
				Content: buildSystemPrompt(
					s.getPrompt("overall", defaultOverallPrompt),
					s.getPrompt("calendar", defaultCalendarPrompt)+s.feedbackContext("calendar"),
				),
			},
			{Role: "user", Content: "Here are my upcoming calendar events:\n\n" + sb.String()},
		})
		if err != nil {
			emit("calendar:error", fmt.Sprintf("Calendar AI summary failed: %v", err))
			calCh <- calResult{sectionData{Error: fmt.Sprintf("LLM error (calendar): %v", err)}}
			return
		}
		emit("calendar:done", "Calendar summary ready.")
		calCh <- calResult{sectionData{HTML: renderMarkdown(reply), Raw: reply}}
	}()

	go func() {
		if ghC == nil {
			emit("github:skip", "GitHub not configured, skipping.")
			ghCh <- ghResult{}
			return
		}
		emit("github:fetch", "Fetching GitHub PRs...")
		prs, err := ghC.ListRecentPRs(ctx, s.githubSince(), s.githubOrgs(), s.githubUsername())
		if err != nil {
			emit("github:error", fmt.Sprintf("GitHub fetch failed: %v", err))
			ghCh <- ghResult{sectionData{Error: fmt.Sprintf("Failed to fetch GitHub PRs: %v", err)}}
			return
		}
		emit("github:llm", fmt.Sprintf("Summarising %d PR(s) with AI...", len(prs)))
		var sb strings.Builder
		if len(prs) == 0 {
			sb.WriteString("No recent pull request activity.")
		} else {
			cutoff24h := time.Now().UTC().Add(-24 * time.Hour)

			// Partition into newly opened (created in last 24h) vs active (older).
			var newPRs, activePRs []github.PullRequest
			for _, pr := range prs {
				if pr.CreatedAt.After(cutoff24h) {
					newPRs = append(newPRs, pr)
				} else {
					activePRs = append(activePRs, pr)
				}
			}

			writePR := func(pr github.PullRequest) {
				status := pr.State
				if pr.MergedAt != nil {
					status = "merged"
				}
				fmt.Fprintf(&sb, "- [%s#%d](%s) %s (%s) by %s — updated %s\n",
					pr.Repo, pr.Number, pr.HTMLURL, pr.Title,
					status, pr.Author,
					pr.UpdatedAt.In(easternLoc).Format("Mon Jan 2 15:04"),
				)
				for _, r := range pr.Reviews {
					fmt.Fprintf(&sb, "  - review by %s: %s (%s)\n",
						r.Author, r.State,
						r.CreatedAt.In(easternLoc).Format("Jan 2 15:04"),
					)
				}
				for _, cm := range pr.Comments {
					fmt.Fprintf(&sb, "  - comment by %s (%s): %s\n",
						cm.Author,
						cm.CreatedAt.In(easternLoc).Format("Jan 2 15:04"),
						cm.Body,
					)
				}
				for _, co := range pr.RecentCommits {
					fmt.Fprintf(&sb, "  - commit %s by %s (%s): %s\n",
						co.SHA, co.Author,
						co.CreatedAt.In(easternLoc).Format("Jan 2 15:04"),
						co.Message,
					)
				}
			}

			if len(newPRs) > 0 {
				sb.WriteString("### New PRs opened today\n")
				for _, pr := range newPRs {
					writePR(pr)
				}
				sb.WriteString("\n")
			}
			if len(activePRs) > 0 {
				sb.WriteString("### Active PRs with recent updates\n")
				for _, pr := range activePRs {
					writePR(pr)
				}
			}
		}
		reply, err := llmC.Chat(ctx, []llm.Message{
			{
				Role: "system",
				Content: buildSystemPrompt(
					s.getPrompt("overall", defaultOverallPrompt),
					s.getPrompt("github", defaultGitHubPrompt)+s.feedbackContext("github"),
				),
			},
			{Role: "user", Content: "Here is my recent GitHub pull request activity:\n\n" + sb.String()},
		})
		if err != nil {
			emit("github:error", fmt.Sprintf("GitHub AI summary failed: %v", err))
			ghCh <- ghResult{sectionData{Error: fmt.Sprintf("LLM error (github): %v", err)}}
			return
		}
		emit("github:done", "GitHub PR summary ready.")
		ghCh <- ghResult{sectionData{HTML: renderMarkdown(reply), Raw: reply}}
	}()

	go func() {
		fmC := s.getFMClient()
		if fmC == nil {
			emit("fastmail:skip", "Fastmail not configured, skipping.")
			fmCh <- fmResult{}
			return
		}
		emit("fastmail:fetch", "Fetching Fastmail inbox...")
		msgs, err := fmC.ListMessages(ctx, 20)
		if err != nil {
			emit("fastmail:error", fmt.Sprintf("Fastmail fetch failed: %v", err))
			fmCh <- fmResult{section: sectionData{Error: fmt.Sprintf("Failed to fetch Fastmail: %v", err)}}
			return
		}
		emit("fastmail:llm", fmt.Sprintf("Summarising %d personal email(s) with AI...", len(msgs)))

		// Build classifyMsg list for low-prio classification.
		classifyMsgs := make([]classifyMsg, len(msgs))
		for i, m := range msgs {
			classifyMsgs[i] = classifyMsg{ID: m.ID, From: m.From, Subject: m.Subject, Preview: m.BodyPreview}
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
		reply, err := llmC.Chat(ctx, []llm.Message{
			{
				Role: "system",
				Content: buildSystemPrompt(
					s.getPrompt("overall", defaultOverallPrompt),
					s.getPrompt("fastmail", defaultFastmailPrompt)+s.feedbackContext("fastmail"),
				),
			},
			{Role: "user", Content: "Here are my recent personal emails:\n\n" + sb.String()},
		})
		if err != nil {
			emit("fastmail:error", fmt.Sprintf("Fastmail AI summary failed: %v", err))
			fmCh <- fmResult{section: sectionData{Error: fmt.Sprintf("LLM error (fastmail): %v", err)}}
			return
		}

		// Classify low-priority messages using the already-fetched list.
		emit("fastmail:classify", "Classifying low-priority personal email...")
		proposed, classErr := classifyLowPriority(ctx, classifyMsgs, llmC)
		var fmLowPrios []lowPrioMsg
		if classErr != nil {
			log.Printf("GenerateBriefing: classify fastmail low-prio: %v", classErr)
		} else {
			ids := filterToKnownIDs(proposed, classifyMsgs)
			idSet := make(map[string]struct{}, len(ids))
			for _, id := range ids {
				idSet[id] = struct{}{}
			}
			for _, m := range msgs {
				if _, ok := idSet[m.ID]; ok {
					fmLowPrios = append(fmLowPrios, lowPrioMsg{
						ID:         m.ID,
						Source:     "fastmail",
						From:       m.From,
						Subject:    m.Subject,
						ReceivedAt: m.ReceivedAt,
					})
				}
			}
		}

		emit("fastmail:done", "Personal email summary ready.")
		fmCh <- fmResult{
			section:  sectionData{HTML: renderMarkdown(reply), Raw: reply},
			lowPrios: fmLowPrios,
		}
	}()

	emailRes := <-emailCh
	calRes := <-calCh
	ghRes := <-ghCh
	fmRes := <-fmCh

	rep := &cachedReport{
		EmailRaw:      emailRes.section.Raw,
		CalendarRaw:   calRes.section.Raw,
		GitHubRaw:     ghRes.section.Raw,
		FastmailRaw:   fmRes.section.Raw,
		EmailError:    emailRes.section.Error,
		CalendarError: calRes.section.Error,
		GitHubError:   ghRes.section.Error,
		FastmailError: fmRes.section.Error,
		LowPrioMsgs:   append(emailRes.lowPrios, fmRes.lowPrios...),
		GeneratedAt:   time.Now().UTC(),
	}
	if err := s.saveLastReport(rep); err != nil {
		log.Printf("save last report: %v", err)
	}
	emit("done", "Briefing complete.")
	return rep, nil
}

// briefingMarkdown formats a cachedReport as a Markdown document suitable for
// sending via ntfy. Sections with errors are omitted.
func briefingMarkdown(rep *cachedReport) string {
	var sb strings.Builder
	date := rep.GeneratedAt.In(easternLoc).Format("2006-01-02")
	fmt.Fprintf(&sb, "# 7 AM Office Summary – %s\n\n", date)
	if rep.EmailRaw != "" {
		fmt.Fprintf(&sb, "## Work Email\n\n%s\n\n", rep.EmailRaw)
	}
	if rep.CalendarRaw != "" {
		fmt.Fprintf(&sb, "## Calendar\n\n%s\n\n", rep.CalendarRaw)
	}
	if rep.GitHubRaw != "" {
		fmt.Fprintf(&sb, "## GitHub PRs\n\n%s\n\n", rep.GitHubRaw)
	}
	if rep.FastmailRaw != "" {
		fmt.Fprintf(&sb, "## Personal Email\n\n%s\n\n", rep.FastmailRaw)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// sendNtfyReport loads the last cached report and sends it to ntfy.
// Returns an error if the topic is not set, no report exists, or the send fails.
func (s *Server) sendNtfyReport(ctx context.Context) error {
	topic := strings.TrimSpace(s.getSetting("ntfy_topic", ""))
	if topic == "" {
		return fmt.Errorf("ntfy topic not configured")
	}
	rep, err := s.loadLastReport()
	if err != nil {
		return fmt.Errorf("load report: %w", err)
	}
	if rep == nil {
		return fmt.Errorf("no report generated yet")
	}
	date := rep.GeneratedAt.In(easternLoc).Format("2006-01-02")
	title := "7 AM Office Update – " + date
	body := briefingMarkdown(rep)
	return ntfy.Send(ctx, topic, title, body)
}

// StartScheduler launches a background goroutine that wakes at 7:00 AM local
// time each day, generates a briefing, and sends it via ntfy.sh. It returns
// immediately; the goroutine runs until ctx is cancelled.
func (s *Server) StartScheduler(ctx context.Context) {
	go func() {
		for {
			next := nextSevenAM(time.Now().In(easternLoc))
			timer := time.NewTimer(time.Until(next))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			topic := strings.TrimSpace(s.getSetting("ntfy_topic", ""))
			if topic == "" {
				log.Println("scheduler: ntfy topic not configured, skipping send")
				continue
			}
			log.Println("scheduler: generating and sending 7 AM briefing via ntfy")
			if _, err := s.GenerateBriefing(ctx); err != nil {
				log.Printf("scheduler: generate briefing failed: %v", err)
				continue
			}
			if err := s.sendNtfyReport(ctx); err != nil {
				log.Printf("scheduler: ntfy send failed: %v", err)
			} else {
				log.Println("scheduler: ntfy send succeeded")
			}
		}
	}()
}

// nextSevenAM returns the next 7:00 AM occurrence strictly in the future
// relative to now. If now is before 7:00 AM today, it returns today at
// 7:00 AM; otherwise it returns tomorrow at 7:00 AM.
func nextSevenAM(now time.Time) time.Time {
	y, mo, d := now.Date()
	loc := now.Location()
	today7 := time.Date(y, mo, d, 7, 0, 0, 0, loc)
	if now.Before(today7) {
		return today7
	}
	return today7.AddDate(0, 0, 1)
}

// generatingTmpl is the page shown while briefing generation is in progress.
// It opens an EventSource to /generate/progress and updates a live step list.
var generatingTmpl = template.Must(template.New("generating").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>officeagent — Generating…</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:system-ui,sans-serif;background:#f5f5f5;color:#1a1a1a;padding:2rem 1rem;line-height:1.6}
.wrap{max-width:720px;margin:0 auto}
header{display:flex;align-items:baseline;gap:1rem;margin-bottom:2rem;border-bottom:2px solid #0078d4;padding-bottom:.75rem}
header h1{font-size:1.4rem;color:#0078d4;font-weight:700;letter-spacing:-.5px}
header span{font-size:.85rem;color:#666;flex:1}
.card{background:#fff;border-radius:10px;padding:1.75rem 2rem;box-shadow:0 1px 4px rgba(0,0,0,.08)}
h2{font-size:1.05rem;margin-bottom:1.25rem;color:#0078d4}
#steps{list-style:none;padding:0;margin:0}
#steps li{display:flex;align-items:flex-start;gap:.75rem;padding:.45rem 0;border-bottom:1px solid #f0f0f0;font-size:.9rem}
#steps li:last-child{border-bottom:none}
.icon{flex-shrink:0;width:1.1em;text-align:center}
.msg{flex:1;color:#333}
.spin{display:inline-block;width:.85em;height:.85em;border:2px solid rgba(0,120,212,.25);border-top-color:#0078d4;border-radius:50%;animation:spin .6s linear infinite;vertical-align:middle}
@keyframes spin{to{transform:rotate(360deg)}}
#progress-bar{height:4px;background:#0078d4;width:0;transition:width .4s ease;border-radius:2px;margin-bottom:1.5rem}
#status-line{font-size:.82rem;color:#888;margin-top:1.25rem}
.done-icon{color:#107c10}
.error-icon{color:#c00}
.skip-icon{color:#aaa}
</style>
</head>
<body>
<div class="wrap">
  <header>
    <h1>officeagent</h1>
    <span>generating briefing…</span>
  </header>
  <div id="progress-bar"></div>
  <div class="card">
    <h2>Generating your morning briefing</h2>
    <ul id="steps"></ul>
    <div id="status-line">Connecting…</div>
  </div>
</div>
<script>
(function() {
  var steps = document.getElementById('steps');
  var bar = document.getElementById('progress-bar');
  var statusLine = document.getElementById('status-line');

  // Progress bar milestones keyed by step prefix.
  var barSteps = {
    'email:fetch': 5, 'email:llm': 15, 'email:done': 25, 'email:error': 25,
    'calendar:fetch': 30, 'calendar:llm': 40, 'calendar:done': 50, 'calendar:error': 50,
    'github:fetch': 55, 'github:llm': 65, 'github:done': 75, 'github:error': 75, 'github:skip': 75,
    'fastmail:fetch': 80, 'fastmail:llm': 88, 'fastmail:done': 96, 'fastmail:error': 96, 'fastmail:skip': 96,
    'done': 100, 'error': 100
  };

  function icon(step) {
    if (step.endsWith(':done')) return '<span class="icon done-icon">&#10003;</span>';
    if (step.endsWith(':error')) return '<span class="icon error-icon">&#10007;</span>';
    if (step.endsWith(':skip')) return '<span class="icon skip-icon">&#8212;</span>';
    if (step === 'done') return '<span class="icon done-icon">&#10003;</span>';
    if (step === 'error') return '<span class="icon error-icon">&#10007;</span>';
    return '<span class="icon"><span class="spin"></span></span>';
  }

  // Track live (in-progress) list items so we can update them to done/error.
  var liveItems = {};

  function addStep(step, msg) {
    var sectionPrefix = step.split(':')[0];
    // If we already have a live item for this section, update it in-place.
    if (liveItems[sectionPrefix] && !step.endsWith(':fetch')) {
      var li = liveItems[sectionPrefix];
      li.querySelector('.icon').outerHTML = icon(step);
      li.querySelector('.msg').textContent = msg;
      if (step.endsWith(':done') || step.endsWith(':error') || step.endsWith(':skip') || step === 'done' || step === 'error') {
        delete liveItems[sectionPrefix];
      }
      return;
    }
    var li = document.createElement('li');
    li.innerHTML = icon(step) + '<span class="msg">' + msg + '</span>';
    steps.appendChild(li);
    if (!step.endsWith(':done') && !step.endsWith(':error') && !step.endsWith(':skip') && step !== 'done' && step !== 'error') {
      liveItems[sectionPrefix] = li;
    }
  }

  var src = new EventSource('/generate/progress');

  src.onopen = function() {
    statusLine.textContent = 'Connected. Waiting for first step…';
  };

  src.onmessage = function(e) {
    var ev;
    try { ev = JSON.parse(e.data); } catch(_) { return; }
    addStep(ev.Step, ev.Message);
    var pct = barSteps[ev.Step];
    if (pct !== undefined) { bar.style.width = pct + '%'; }
    statusLine.textContent = ev.Message;
    if (ev.Step === 'done') {
      statusLine.textContent = 'Done! Redirecting…';
      src.close();
      setTimeout(function(){ window.location.href = '/'; }, 700);
    } else if (ev.Step === 'error') {
      statusLine.textContent = 'Generation failed. Redirecting…';
      src.close();
      setTimeout(function(){ window.location.href = '/'; }, 1500);
    }
  };

  src.onerror = function() {
    // If the SSE stream closes (server closed it normally after 'done'),
    // the browser fires onerror. Only treat it as a real error if we haven't
    // already received 'done'.
    if (bar.style.width === '100%') return;
    statusLine.textContent = 'Lost connection. Redirecting to briefing…';
    src.close();
    setTimeout(function(){ window.location.href = '/'; }, 1000);
  };
})();
</script>
</body>
</html>`))

func (s *Server) handleGenerate(w http.ResponseWriter, r *http.Request) {
	if !s.auth.IsAuthenticated(r.Context()) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	if s.getLLM() == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	// Reset the progress bus NOW, before the redirect, so that any client
	// subscribing to /generate/progress after this redirect cannot receive the
	// stale "done" event from the previous run. GenerateBriefing also calls
	// reset() at its start, but there is a race window between the goroutine
	// launch and the client connecting that this early reset closes.
	s.progress.reset()
	// Launch generation in the background so the browser can navigate to the
	// progress page immediately. Use a detached context so cancelling the
	// request doesn't abort generation.
	go func() {
		if _, err := s.GenerateBriefing(context.Background()); err != nil {
			log.Printf("handleGenerate (background): %v", err)
			s.progress.publish(progressEvent{Step: "error", Message: fmt.Sprintf("Generation failed: %v", err)})
		}
	}()
	http.Redirect(w, r, "/generating", http.StatusSeeOther)
}

// handleGeneratingPage serves the progress page shown while a briefing is
// being generated. It uses an EventSource to receive SSE events from
// /generate/progress and auto-redirects to / when done.
func (s *Server) handleGeneratingPage(w http.ResponseWriter, r *http.Request) {
	if !s.auth.IsAuthenticated(r.Context()) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := generatingTmpl.Execute(w, nil); err != nil {
		log.Printf("generating template: %v", err)
	}
}

// handleGenerateProgress is the SSE endpoint that streams progress events while
// a briefing is being generated.
func (s *Server) handleGenerateProgress(w http.ResponseWriter, r *http.Request) {
	if !s.auth.IsAuthenticated(r.Context()) {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := s.progress.subscribe()
	defer s.progress.unsubscribe(ch)

	// Send a keepalive comment immediately so the browser knows the connection
	// is alive.
	_, _ = fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			// Encode message as JSON so the client can parse step + message.
			payload, _ := json.Marshal(ev)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
			flusher.Flush()
			// Close the SSE stream after the terminal events so the client
			// can do its redirect without waiting for a timeout.
			if ev.Step == "done" || ev.Step == "error" {
				return
			}
		}
	}
}

// handleSendReport handles POST /send-report — triggers an immediate ntfy send.
func (s *Server) handleSendReport(w http.ResponseWriter, r *http.Request) {
	if !s.auth.IsAuthenticated(r.Context()) {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}
	if err := s.sendNtfyReport(r.Context()); err != nil {
		http.Error(w, fmt.Sprintf("send report: %v", err), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "sent"})
}

// --- connect page ---

var connectTmpl = template.Must(template.New("connect").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>officeagent — Connect</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:system-ui,sans-serif;background:#f5f5f5;color:#1a1a1a;padding:2rem 1rem;line-height:1.6}
.wrap{max-width:720px;margin:0 auto}
header{display:flex;align-items:baseline;gap:1rem;margin-bottom:2rem;border-bottom:2px solid #0078d4;padding-bottom:.75rem}
header h1{font-size:1.4rem;color:#0078d4;font-weight:700;letter-spacing:-.5px}
header a{font-size:.82rem;color:#888;text-decoration:none}
header a:hover{color:#0078d4}
.service{background:#fff;border-radius:10px;padding:1.5rem 2rem;box-shadow:0 1px 4px rgba(0,0,0,.08);margin-bottom:1rem;display:flex;align-items:center;gap:1.5rem}
.service-info{flex:1}
.service-name{font-weight:700;font-size:1rem;margin-bottom:.2rem}
.service-desc{font-size:.82rem;color:#666}
.badge{display:inline-block;border-radius:4px;padding:.2rem .55rem;font-size:.78rem;font-weight:700;margin-top:.35rem}
.badge-ok{background:#dff6dd;color:#107c10}
.badge-warn{background:#fff4ce;color:#b86800}
.badge-fail{background:#fde7e9;color:#c00}
.action a{display:inline-block;padding:.45rem 1rem;border-radius:6px;font-size:.85rem;font-weight:600;text-decoration:none;background:#0078d4;color:#fff;white-space:nowrap}
.action a:hover{background:#006cbd}
.action-none{font-size:.82rem;color:#aaa}
.btn-disconnect{background:#c00;color:#fff;border:none;border-radius:6px;padding:.45rem 1rem;font-size:.85rem;font-weight:600;cursor:pointer;white-space:nowrap}
.btn-disconnect:hover{background:#a00}
.expiry{display:block;font-size:.78rem;color:#666;margin-top:.25rem}
.expiry-warn{color:#b86800;font-weight:600}
.divider{margin:1.5rem 0 .75rem;font-size:.72rem;font-weight:700;letter-spacing:.08em;text-transform:uppercase;color:#aaa}
footer{margin-top:1.5rem;font-size:.78rem;color:#aaa;text-align:right}
footer a{color:#aaa;text-decoration:none}
footer a:hover{color:#0078d4}
</style>
</head>
<body>
<div class="wrap">
  <header>
    <h1>officeagent</h1>
    <span style="flex:1">connect</span>
    <a href="/">← Morning briefing</a>
  </header>

  <div class="divider">Microsoft</div>

  <div class="service">
    <div class="service-info">
      <div class="service-name">Microsoft Graph (Office 365)</div>
      <div class="service-desc">Email and calendar access via OAuth</div>
      {{if .MSGraphAuth}}
      <span class="badge badge-ok">&#10003; Authenticated</span>
      {{if .MSGraphExpiry}}<span class="expiry{{if .MSGraphExpired}} expiry-warn{{end}}">Token expires {{.MSGraphExpiry}}</span>{{end}}
      {{else if not .MSGraphClientID}}
      <span class="badge badge-fail">&#10007; Azure client ID not set</span>
      {{else}}
      <span class="badge badge-warn">&#9888; Not signed in</span>
      {{end}}
    </div>
    <div class="action">
      {{if .MSGraphAuth}}
      <a href="/login">Re-authenticate</a>
      <form method="POST" action="/disconnect" style="display:inline;margin-left:.5rem">
        <button type="submit" class="btn-disconnect">Disconnect</button>
      </form>
      {{else if not .MSGraphClientID}}
      <a href="/settings">Configure in Settings</a>
      {{else}}
      <a href="/login">Sign in</a>
      {{end}}
    </div>
  </div>

  <div class="divider">GitHub</div>

  <div class="service">
    <div class="service-info">
      <div class="service-name">GitHub / LLM</div>
      <div class="service-desc">GitHub PR activity and AI summaries via GitHub Copilot API</div>
      {{if .GitHubToken}}
      <span class="badge badge-ok">&#10003; Token configured</span>
      {{else}}
      <span class="badge badge-fail">&#10007; Token not set</span>
      {{end}}
    </div>
    <div class="action">
      <a href="/settings#github_token">{{if .GitHubToken}}Update token{{else}}Configure in Settings{{end}}</a>
    </div>
  </div>

  <div class="divider">Personal Email</div>

  <div class="service">
    <div class="service-info">
      <div class="service-name">Fastmail</div>
      <div class="service-desc">Personal inbox summaries via Fastmail JMAP API</div>
      {{if .FastmailToken}}
      <span class="badge badge-ok">&#10003; Token configured</span>
      {{else}}
      <span class="badge badge-warn">&#9888; Token not set (optional)</span>
      {{end}}
    </div>
    <div class="action">
      <a href="/settings#fastmail_token">{{if .FastmailToken}}Update token{{else}}Configure in Settings{{end}}</a>
    </div>
  </div>

  <footer><a href="/doctor">Technical diagnostics →</a></footer>
</div>
</body>
</html>`))

type connectData struct {
	MSGraphAuth      bool      // authenticated with Microsoft Graph
	MSGraphClientID  bool      // Azure client ID is configured
	MSGraphExpiry    string    // formatted token expiry (empty if not authenticated)
	MSGraphExpired   bool      // true if the token is past its expiry time
	GitHubToken      bool      // GitHub token is configured
	FastmailToken    bool      // Fastmail API token is configured
}

func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	expStr := ""
	expired := false
	if exp, ok := s.auth.TokenExpiry(r.Context()); ok {
		expStr = exp.In(easternLoc).Format("Jan 2 2006 3:04 PM MST")
		expired = time.Now().After(exp)
	}
	data := connectData{
		MSGraphAuth:     s.auth.IsAuthenticated(r.Context()),
		MSGraphClientID: s.effectiveAzureClientID() != "",
		MSGraphExpiry:   expStr,
		MSGraphExpired:  expired,
		GitHubToken:     s.effectiveGitHubToken() != "",
		FastmailToken:   s.effectiveFastmailToken() != "",
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := connectTmpl.Execute(w, data); err != nil {
		log.Printf("connect template: %v", err)
	}
}

// handleDisconnect handles POST /disconnect — clears the stored Graph OAuth
// token and redirects to /connect so the user can re-authenticate.
func (s *Server) handleDisconnect(w http.ResponseWriter, r *http.Request) {
	if err := s.auth.ClearToken(); err != nil {
		http.Error(w, fmt.Sprintf("clear token: %v", err), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/connect", http.StatusSeeOther)
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
	if s.effectiveAzureClientID() == "" {
		if err := loginTmpl.Execute(w, loginData{ClientIDMissing: true}); err != nil {
			log.Printf("login template: %v", err)
		}
		return
	}

	// Always proceed to the OAuth flow — even when a token is already stored.
	// prompt=consent (set in AuthCodeURL) ensures Azure re-presents the consent
	// screen so newly-added scopes are granted. Skipping here was the reason
	// re-authentication retained the old (narrower) scope set.

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

	http.Redirect(w, r, "/connect", http.StatusFound)
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
	llmC := s.getLLM()
	if llmC == nil {
		http.Error(w, "LLM not configured — set GITHUB_TOKEN", http.StatusServiceUnavailable)
		return
	}
	reply, err := llmC.Chat(r.Context(), []llm.Message{
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
    <a href="/connect">Connect</a>
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
	Name    string
	ok      bool
	warn    bool // optional check; not a failure if unconfigured
	Detail  string
	Latency time.Duration
}

func (c checkResult) Status() string {
	if c.ok {
		return "OK"
	}
	if c.warn {
		return "WARN"
	}
	return "FAIL"
}

func (c checkResult) StatusClass() string {
	if c.ok {
		return "ok"
	}
	if c.warn {
		return "warn"
	}
	return "fail"
}

func (c checkResult) StatusIcon() string {
	if c.ok {
		return "✓"
	}
	if c.warn {
		return "⚠"
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
	checks := make([]checkResult, 5)
	ch := make(chan result, 5)

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
		llmC := s.getLLM()
		if llmC == nil {
			cr.Detail = "not configured — set GITHUB_TOKEN"
			cr.Latency = time.Since(start)
			ch <- result{3, cr}
			return
		}
		_, err := llmC.Chat(ctx, []llm.Message{
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

	// Check 4: Fastmail JMAP
	go func() {
		cr := checkResult{Name: "Fastmail (JMAP)"}
		start := time.Now()
		fmC := s.getFMClient()
		if fmC == nil {
			cr.warn = true
			cr.Detail = "not configured — add Fastmail token via Settings (optional)"
			cr.Latency = time.Since(start)
			ch <- result{4, cr}
			return
		}
		msgs, err := fmC.ListMessages(ctx, 1)
		cr.Latency = time.Since(start)
		if err != nil {
			cr.Detail = fmt.Sprintf("ListMessages failed: %v", err)
		} else {
			cr.ok = true
			cr.Detail = fmt.Sprintf("OK (%d message(s) accessible)", len(msgs))
			// Check that the token has write access (needed for mail moving).
			if checker, ok := fmC.(fastmailReadOnlyChecker); ok {
				if readOnly, roErr := checker.IsReadOnly(ctx); roErr == nil && readOnly {
					cr.ok = false
					cr.warn = true
					cr.Detail += " — token is read-only; mail moving will fail. Regenerate the Fastmail token with full (read+write) access."
				}
			}
		}
		ch <- result{4, cr}
	}()

	for i := 0; i < 5; i++ {
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

// --- reports history ---

var reportsListTmpl = template.Must(template.New("reports-list").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>officeagent — Report History</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:system-ui,sans-serif;background:#f5f5f5;color:#1a1a1a;padding:2rem 1rem;line-height:1.6}
.wrap{max-width:720px;margin:0 auto}
header{display:flex;align-items:baseline;gap:1rem;margin-bottom:2rem;border-bottom:2px solid #0078d4;padding-bottom:.75rem}
header h1{font-size:1.4rem;color:#0078d4;font-weight:700;letter-spacing:-.5px}
header a{font-size:.82rem;color:#888;text-decoration:none}
header a:hover{color:#0078d4}
.card{background:#fff;border-radius:10px;padding:1.75rem 2rem;box-shadow:0 1px 4px rgba(0,0,0,.08)}
ul{list-style:none;padding:0;margin:0}
li{border-bottom:1px solid #f0f0f0;padding:.6rem 0;display:flex;align-items:baseline;gap:1rem}
li:last-child{border-bottom:none}
li a{color:#0078d4;text-decoration:none;font-size:.9rem}
li a:hover{text-decoration:underline}
li span{font-size:.78rem;color:#aaa}
.empty{color:#888;font-size:.9rem}
</style>
</head>
<body>
<div class="wrap">
  <header>
    <h1>officeagent</h1>
    <span style="flex:1">report history</span>
    <a href="/">← Morning briefing</a>
  </header>
  <div class="card">
    {{if .Reports}}
    <ul>
      {{range .Reports}}
      <li>
        <a href="/reports/{{.ID}}">{{.GeneratedAt}}</a>
        <span>#{{.ID}}</span>
      </li>
      {{end}}
    </ul>
    {{else}}
    <p class="empty">No historical reports yet. Generate a briefing to start building history.</p>
    {{end}}
  </div>
</div>
</body>
</html>`))

type reportsListItem struct {
	ID          int64
	GeneratedAt string
}

type reportsListData struct {
	Reports []reportsListItem
}

func (s *Server) handleReportsList(w http.ResponseWriter, r *http.Request) {
	if !s.auth.IsAuthenticated(r.Context()) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	if s.store == nil {
		http.Error(w, "store not initialised", http.StatusInternalServerError)
		return
	}
	reps, err := s.store.ListReports()
	if err != nil {
		http.Error(w, fmt.Sprintf("list reports: %v", err), http.StatusInternalServerError)
		return
	}
	items := make([]reportsListItem, len(reps))
	for i, rep := range reps {
		items[i] = reportsListItem{
			ID:          rep.ID,
			GeneratedAt: rep.GeneratedAt.In(easternLoc).Format("Mon Jan 2 2006 3:04 PM MST"),
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := reportsListTmpl.Execute(w, reportsListData{Reports: items}); err != nil {
		log.Printf("reports-list template: %v", err)
	}
}

var reportViewTmpl = template.Must(template.New("report-view").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>officeagent — Report {{.GeneratedAt}}</title>
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
</style>
</head>
<body>
<div class="wrap">
  <header>
    <h1>officeagent</h1>
    <span>{{.GeneratedAt}}</span>
    <a href="/reports">← History</a>
    <a href="/">Home</a>
  </header>
  {{if .Email.Error}}<div class="error">{{.Email.Error}}</div>
  {{else if .Email.HTML}}
  <div class="section">
    <div class="section-title">Email</div>
    <div class="card">{{.Email.HTML}}</div>
  </div>
  {{end}}
  {{if .Calendar.Error}}<div class="error">{{.Calendar.Error}}</div>
  {{else if .Calendar.HTML}}
  <div class="section">
    <div class="section-title">Calendar</div>
    <div class="card">{{.Calendar.HTML}}</div>
  </div>
  {{end}}
  {{if or .GitHub.Error .GitHub.HTML}}
  <div class="section">
    <div class="section-title">GitHub PRs</div>
    {{if .GitHub.Error}}<div class="error">{{.GitHub.Error}}</div>
    {{else}}<div class="card">{{.GitHub.HTML}}</div>{{end}}
  </div>
  {{end}}
  {{if or .Fastmail.Error .Fastmail.HTML}}
  <div class="section">
    <div class="section-title">Personal Email (Fastmail)</div>
    {{if .Fastmail.Error}}<div class="error">{{.Fastmail.Error}}</div>
    {{else}}<div class="card">{{.Fastmail.HTML}}</div>{{end}}
  </div>
  {{end}}
</div>
</body>
</html>`))

type reportViewData struct {
	GeneratedAt string
	Email       sectionData
	Calendar    sectionData
	GitHub      sectionData
	Fastmail    sectionData
}

func (s *Server) handleReportView(w http.ResponseWriter, r *http.Request) {
	if !s.auth.IsAuthenticated(r.Context()) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	if s.store == nil {
		http.Error(w, "store not initialised", http.StatusInternalServerError)
		return
	}
	rep, err := s.store.GetReport(id)
	if err != nil {
		http.Error(w, fmt.Sprintf("get report: %v", err), http.StatusInternalServerError)
		return
	}
	if rep == nil {
		http.NotFound(w, r)
		return
	}
	var cached cachedReport
	if err := json.Unmarshal([]byte(rep.Content), &cached); err != nil {
		http.Error(w, fmt.Sprintf("decode report: %v", err), http.StatusInternalServerError)
		return
	}
	data := reportViewData{
		GeneratedAt: rep.GeneratedAt.In(easternLoc).Format("Mon Jan 2 2006 3:04 PM MST"),
		Email:       sectionDataFromCache(cached.EmailRaw, cached.EmailError),
		Calendar:    sectionDataFromCache(cached.CalendarRaw, cached.CalendarError),
		GitHub:      sectionDataFromCache(cached.GitHubRaw, cached.GitHubError),
		Fastmail:    sectionDataFromCache(cached.FastmailRaw, cached.FastmailError),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := reportViewTmpl.Execute(w, data); err != nil {
		log.Printf("report-view template: %v", err)
	}
}
