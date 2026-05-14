package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/robfig/cron/v3"

	"recipe-bot/internal/ai"
	"recipe-bot/internal/config"
	"recipe-bot/internal/pipeline"
	"recipe-bot/internal/spoonacular"
	"recipe-bot/internal/storage"
	"recipe-bot/internal/telegram"
)

func main() {
	log.SetFlags(log.LstdFlags)

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	prompt, err := os.ReadFile(cfg.PromptPath)
	if err != nil {
		log.Fatalf("read prompt file %q: %v", cfg.PromptPath, err)
	}

	store, err := storage.New(cfg.DBPath)
	if err != nil {
		log.Fatalf("storage: %v", err)
	}
	if err := os.MkdirAll(cfg.ImageDir, 0o755); err != nil {
		log.Fatalf("image dir: %v", err)
	}

	// Build the API clients.
	spoonClient := spoonacular.New(cfg.SpoonacularKey)
	aiClient := ai.New(cfg.AnthropicKey, cfg.AIModel, cfg.AIMaxTokens)
	tgClient := telegram.New(cfg.TelegramToken, cfg.TelegramChat)

	// Build the two pipeline stages.
	fetcher := pipeline.NewFetcher(
		spoonClient, aiClient, store,
		string(prompt), cfg.ImageDir, cfg.FetchBatchSize,
	)
	publisher := pipeline.NewPublisher(store, tgClient)

	// On a fresh start the queue is empty, so run the fetcher once up front —
	// otherwise the first scheduled publish would have nothing to send.
	if store.CountReady() == 0 {
		log.Printf("[main] queue empty on startup — running fetcher once")
		fetcher.Run()
	}

	// Schedule both stages. Cron expressions are standard 5-field.
	c := cron.New()
	if _, err := c.AddFunc(cfg.FetchCron, fetcher.Run); err != nil {
		log.Fatalf("schedule fetcher (%q): %v", cfg.FetchCron, err)
	}
	if _, err := c.AddFunc(cfg.PublishCron, publisher.Run); err != nil {
		log.Fatalf("schedule publisher (%q): %v", cfg.PublishCron, err)
	}
	c.Start()
	log.Printf("[main] running — fetch %q, publish %q", cfg.FetchCron, cfg.PublishCron)

	// Block until interrupted, then stop the scheduler cleanly.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	log.Printf("[main] shutting down")
	<-c.Stop().Done()
	log.Printf("[main] stopped")
}
