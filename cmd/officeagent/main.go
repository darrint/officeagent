package main

import (
	"context"
	"log"
	"strings"
	_ "time/tzdata" // embed IANA timezone database for Windows

	"github.com/darrint/officeagent/internal/activitylog"
	"github.com/darrint/officeagent/internal/config"
	"github.com/darrint/officeagent/internal/fastmail"
	"github.com/darrint/officeagent/internal/github"
	"github.com/darrint/officeagent/internal/graph"
	"github.com/darrint/officeagent/internal/llm"
	"github.com/darrint/officeagent/internal/server"
	"github.com/darrint/officeagent/internal/store"
)

func main() {
	cfg := config.Default()

	st, err := store.New(cfg.DBPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			log.Printf("close store: %v", err)
		}
	}()

	// Open the NDJSON activity log alongside the SQLite database.
	actLogPath := strings.TrimSuffix(cfg.DBPath, ".db") + ".activity.ndjson"
	alog, err := activitylog.New(actLogPath)
	if err != nil {
		log.Printf("warning: could not open activity log %s: %v — logging disabled", actLogPath, err)
		alog = activitylog.NewDiscard()
	}
	defer func() {
		if err := alog.Close(); err != nil {
			log.Printf("close activity log: %v", err)
		}
	}()

	// Prefer stored tokens over env-var values so the Settings page is the
	// authoritative source.  Env vars still work as a dev fallback.
	if v, err := st.Get("setting.github_token"); err == nil && v != "" {
		cfg.GitHubToken = v
	}
	if v, err := st.Get("setting.azure_client_id"); err == nil && v != "" {
		cfg.AzureClientID = v
	}

	auth := graph.NewAuth(graph.AuthConfig{
		ClientID: cfg.AzureClientID,
		TenantID: cfg.AzureTenantID,
	}, st)
	client := graph.NewClient(auth)
	client.SetTransport(&activitylog.Transport{Subsystem: "graph", Log: alog})

	var llmClient *llm.Client
	var ghClient *github.Client
	if cfg.GitHubToken != "" {
		llmClient = llm.NewClient(cfg.GitHubToken, cfg.LLMModel)
		llmClient.SetTransport(&activitylog.Transport{Subsystem: "llm", Log: alog})
		ghClient = github.NewClient(cfg.GitHubToken)
		ghClient.SetTransport(&activitylog.Transport{Subsystem: "github", Log: alog})
	} else {
		log.Println("warning: GITHUB_TOKEN not set and no token in settings — LLM features disabled; add token via Settings page")
	}

	var fmClient *fastmail.Client
	if v, err := st.Get("setting.fastmail_token"); err == nil && v != "" {
		fmClient = fastmail.NewClient(v)
		fmClient.SetTransport(&activitylog.Transport{Subsystem: "fastmail", Log: alog})
	}

	ctx := context.Background()
	srv := server.New(cfg, auth, client, llmClient, ghClient, fmClient, st,
		server.WithActivityLog(alog),
	)
	srv.StartScheduler(ctx)
	if err := srv.Run(); err != nil {
		log.Fatal(err)
	}
}
