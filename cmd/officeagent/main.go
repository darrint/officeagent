package main

import (
	"log"
	_ "time/tzdata" // embed IANA timezone database for Windows

	"github.com/darrint/officeagent/internal/config"
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

	var llmClient *llm.Client
	var ghClient *github.Client
	if cfg.GitHubToken != "" {
		llmClient = llm.NewClient(cfg.GitHubToken, cfg.LLMModel)
		ghClient = github.NewClient(cfg.GitHubToken)
	} else {
		log.Println("warning: GITHUB_TOKEN not set and no token in settings — LLM features disabled; add token via Settings page")
	}

	srv := server.New(cfg, auth, client, llmClient, ghClient, st)
	if err := srv.Run(); err != nil {
		log.Fatal(err)
	}
}
