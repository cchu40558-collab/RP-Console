package main

import (
	"log"
	"net/http"
	"os"
	"strings"

	"relay-central/internal/central"
)

func main() {
	password := os.Getenv("CENTRAL_ADMIN_PASSWORD")
	key := os.Getenv("CENTRAL_MASTER_KEY")
	if password == "" || key == "" {
		log.Fatal("CENTRAL_ADMIN_PASSWORD and CENTRAL_MASTER_KEY must both be set")
	}

	dataDir := envOr("CENTRAL_DATA_DIR", "./data")
	listen := envOr("CENTRAL_LISTEN_ADDR", "127.0.0.1:2053")
	allowPrivate := strings.EqualFold(os.Getenv("CENTRAL_ALLOW_PRIVATE_NODES"), "true")

	app, err := central.New(central.Config{
		DataDir:           dataDir,
		AdminPassword:     password,
		MasterKey:         key,
		AllowPrivateNodes: allowPrivate,
	})
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("RP Console v%s listening on %s", central.Version, listen)
	log.Fatal(http.ListenAndServe(listen, app.Handler()))
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
