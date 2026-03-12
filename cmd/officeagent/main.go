package main

import (
	"log"

	"github.com/darrint/officeagent/internal/config"
	"github.com/darrint/officeagent/internal/graph"
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

	auth := graph.NewAuth(graph.AuthConfig{
		ClientID: cfg.AzureClientID,
		TenantID: cfg.AzureTenantID,
	}, st)
	client := graph.NewClient(auth)

	srv := server.New(cfg, auth, client)
	if err := srv.Run(); err != nil {
		log.Fatal(err)
	}
}
