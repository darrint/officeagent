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

	clientMu      sync.RWMutex
	pendingMu     sync.Mutex
	pendingLogins map[string]pendingLogin // state -> pending
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
func New(cfg *config.Config, auth *graph.Auth, client *graph.Client, llmClient *llm.Client, ghClient *github.Client, fmClient *fastmail.Client, st *store.Store) *Server {
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
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("GET /connect", s.handleConnect)
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
#gen-loading-bar{position:fixed;top:0;left:0;height:3px;background:#0078d4;width:0;transition:width .3s ease;z-index:9999}
</style>
<script>
function startGenerate(btn) {
  var bar = document.getElementById('gen-loading-bar');
  if (bar) { bar.style.width = '70%'; }
  setTimeout(function() {
    btn.disabled = true;
    btn.innerHTML = '<span class="spinner"></span>Generating\u2026';
  }, 0);
}
function archiveLowPrio() {
  var btn = document.getElementById('archive-btn');
  var out = document.getElementById('archive-result');
  btn.disabled = true;
  btn.innerHTML = '<span class="spinner" style="border-color:rgba(0,0,0,.2);border-top-color:#0078d4"></span>Archiving\u2026';
  fetch('/archive-lowprio', {method:'POST'})
    .then(function(r){ return r.json(); })
    .then(function(d){
      btn.disabled = false;
      btn.innerHTML = 'Move Low-Priority Mail';
      var parts = [];
      if (d.fastmail_moved > 0) parts.push('Fastmail: ' + d.fastmail_moved + ' archived');
      if (d.graph_moved > 0) parts.push('Office 365: ' + d.graph_moved + ' archived');
      if (d.fastmail_error) parts.push('Fastmail error: ' + d.fastmail_error);
      if (d.graph_error) parts.push('Office 365 error: ' + d.graph_error);
      if (parts.length === 0) parts.push('No low-priority mail found.');
      out.textContent = parts.join(' \u00b7 ');
    })
    .catch(function(e){
      btn.disabled = false;
      btn.innerHTML = 'Move Low-Priority Mail';
      out.textContent = 'Error: ' + e;
    });
}
function sendReport() {
  var btn = document.getElementById('send-report-btn');
  var out = document.getElementById('archive-result');
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
<div id="gen-loading-bar"></div>
<div class="wrap">
  <header>
    <h1>officeagent</h1>
    <span>morning briefing</span>
    <a href="/connect">Connect</a>
    <a href="/settings">Settings</a>
  </header>
  {{if .FatalError}}
  <div class="error">{{.FatalError}}</div>
  {{else if .GeneratedAt}}
  <div class="gen-bar">
    <span>Generated {{.GeneratedAt}}</span>
    <form method="POST" action="/generate"><button type="submit" onclick="startGenerate(this)">Regenerate</button></form>
    <button type="button" id="archive-btn" onclick="archiveLowPrio()">Move Low-Priority Mail</button>
    <button type="button" id="send-report-btn" onclick="sendReport()" style="background:#107c10">Send Now</button>
  </div>
  <div id="archive-result" style="font-size:.82rem;color:#555;margin-bottom:.75rem;padding:0 1rem"></div>
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
  <details>
    <summary>Raw JSON</summary>
    <pre>{{.RawJSON}}</pre>
  </details>
  {{else}}
  <div class="empty-state">
    <p>No briefing generated yet.</p>
    <form method="POST" action="/generate"><button type="submit" onclick="startGenerate(this)">Generate Briefing</button></form>
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
      <p class="hint">Fastmail API token with mail scope for personal inbox summaries. Generate one at <code>app.fastmail.com/settings/security/tokens</code>. Never echoed back to the browser.</p>
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

// archiveFastmailLowPrio classifies and moves low-priority Fastmail messages.
// Returns count moved and error string (empty on success).
func (s *Server) archiveFastmailLowPrio(ctx context.Context, llmC llmService) (int, string) {
	fmC, ok := s.getFMClient().(fastmailMoverService)
	if !ok {
		return 0, "" // Fastmail not configured or client doesn't support moving
	}
	folderName := s.getSetting("fastmail_lowprio_folder", "Low Priority")

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
	ids := filterToKnownIDs(proposed, msgs)
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

// archiveGraphLowPrio classifies and moves low-priority Graph (Office 365) messages.
// Returns count moved and error string (empty on success).
func (s *Server) archiveGraphLowPrio(ctx context.Context, llmC llmService) (int, string) {
	gC, ok := s.client.(graphMoverService)
	if !ok {
		return 0, "" // Graph not configured or client doesn't support moving
	}
	folderName := s.getSetting("graph_lowprio_folder", "Low Priority")

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
	ids := filterToKnownIDs(proposed, msgs)
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
	EmailRaw       string    `json:"email_raw"`
	CalendarRaw    string    `json:"calendar_raw"`
	GitHubRaw      string    `json:"github_raw"`
	FastmailRaw    string    `json:"fastmail_raw,omitempty"`
	EmailError     string    `json:"email_error,omitempty"`
	CalendarError  string    `json:"calendar_error,omitempty"`
	GitHubError    string    `json:"github_error,omitempty"`
	FastmailError  string    `json:"fastmail_error,omitempty"`
	GeneratedAt    time.Time `json:"generated_at"`
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

// saveLastReport persists the cached report to the store.
func (s *Server) saveLastReport(rep *cachedReport) error {
	if s.store == nil {
		return nil
	}
	b, err := json.Marshal(rep)
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}
	return s.store.Set(reportStoreKey, string(b))
}

// GenerateBriefing fetches data from all configured sources, asks the LLM for
// summaries of each section, saves the result to the store, and returns it.
// It is safe to call from background goroutines (uses the supplied context).
func (s *Server) GenerateBriefing(ctx context.Context) (*cachedReport, error) {
	llmC := s.getLLM()
	ghC := s.getGHClient()
	if llmC == nil {
		return nil, fmt.Errorf("LLM not configured")
	}

	type emailResult struct{ section sectionData }
	type calResult struct{ section sectionData }
	type ghResult struct{ section sectionData }
	type fmResult struct{ section sectionData }

	emailCh := make(chan emailResult, 1)
	calCh := make(chan calResult, 1)
	ghCh := make(chan ghResult, 1)
	fmCh := make(chan fmResult, 1)

	go func() {
		msgs, err := s.client.ListMessages(ctx, 20)
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
			emailCh <- emailResult{sectionData{Error: fmt.Sprintf("LLM error (email): %v", err)}}
			return
		}
		emailCh <- emailResult{sectionData{HTML: renderMarkdown(reply), Raw: reply}}
	}()

	go func() {
		events, err := s.client.ListEvents(ctx, 20)
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
			calCh <- calResult{sectionData{Error: fmt.Sprintf("LLM error (calendar): %v", err)}}
			return
		}
		calCh <- calResult{sectionData{HTML: renderMarkdown(reply), Raw: reply}}
	}()

	go func() {
		if ghC == nil {
			ghCh <- ghResult{}
			return
		}
		prs, err := ghC.ListRecentPRs(ctx, s.githubSince(), s.githubOrgs(), s.githubUsername())
		if err != nil {
			ghCh <- ghResult{sectionData{Error: fmt.Sprintf("Failed to fetch GitHub PRs: %v", err)}}
			return
		}
		var sb strings.Builder
		if len(prs) == 0 {
			sb.WriteString("No recent pull request activity.")
		} else {
			for _, pr := range prs {
				status := pr.State
				if pr.MergedAt != nil {
					status = "merged"
				}
				fmt.Fprintf(&sb, "- [%s#%d](%s) %s (%s) — updated %s\n",
					pr.Repo, pr.Number, pr.HTMLURL, pr.Title,
					status,
					pr.UpdatedAt.In(easternLoc).Format("Mon Jan 2"),
				)
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
			ghCh <- ghResult{sectionData{Error: fmt.Sprintf("LLM error (github): %v", err)}}
			return
		}
		ghCh <- ghResult{sectionData{HTML: renderMarkdown(reply), Raw: reply}}
	}()

	go func() {
		fmC := s.getFMClient()
		if fmC == nil {
			fmCh <- fmResult{}
			return
		}
		msgs, err := fmC.ListMessages(ctx, 20)
		if err != nil {
			fmCh <- fmResult{sectionData{Error: fmt.Sprintf("Failed to fetch Fastmail: %v", err)}}
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
			fmCh <- fmResult{sectionData{Error: fmt.Sprintf("LLM error (fastmail): %v", err)}}
			return
		}
		fmCh <- fmResult{sectionData{HTML: renderMarkdown(reply), Raw: reply}}
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
		GeneratedAt:   time.Now().UTC(),
	}
	if err := s.saveLastReport(rep); err != nil {
		log.Printf("save last report: %v", err)
	}
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

func (s *Server) handleGenerate(w http.ResponseWriter, r *http.Request) {
	if !s.auth.IsAuthenticated(r.Context()) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	if s.getLLM() == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if _, err := s.GenerateBriefing(r.Context()); err != nil {
		log.Printf("handleGenerate: %v", err)
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
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
      {{else if not .MSGraphClientID}}
      <span class="badge badge-fail">&#10007; Azure client ID not set</span>
      {{else}}
      <span class="badge badge-warn">&#9888; Not signed in</span>
      {{end}}
    </div>
    <div class="action">
      {{if .MSGraphAuth}}
      <span class="action-none">Connected</span>
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
	MSGraphAuth     bool // authenticated with Microsoft Graph
	MSGraphClientID bool // Azure client ID is configured
	GitHubToken     bool // GitHub token is configured
	FastmailToken   bool // Fastmail API token is configured
}

func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	data := connectData{
		MSGraphAuth:     s.auth.IsAuthenticated(r.Context()),
		MSGraphClientID: s.effectiveAzureClientID() != "",
		GitHubToken:     s.effectiveGitHubToken() != "",
		FastmailToken:   s.effectiveFastmailToken() != "",
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := connectTmpl.Execute(w, data); err != nil {
		log.Printf("connect template: %v", err)
	}
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
