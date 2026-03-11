package main

import (
	"log"

	"github.com/darrint/officeagent/internal/config"
	"github.com/darrint/officeagent/internal/server"
)

func main() {
	cfg := config.Default()
	srv := server.New(cfg)
	if err := srv.Run(); err != nil {
		log.Fatal(err)
	}
}
