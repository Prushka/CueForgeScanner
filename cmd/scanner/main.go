package main

import (
	"context"
	"log"
	"math/rand"
	"net/http"
	"time"

	"CueForgeScanner/internal/config"
	"CueForgeScanner/internal/cueforge"
	"CueForgeScanner/internal/scanner"
)

func main() {
	if err := run(context.Background()); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	client := &http.Client{}
	if cfg.RequestTimeout > 0 {
		client.Timeout = cfg.RequestTimeout
	}

	languages, err := cueforge.FetchLanguages(ctx, client, cfg.CueForgeBaseURL)
	if err != nil {
		return err
	}

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	return scanner.Run(ctx, client, rng, cfg, languages)
}
