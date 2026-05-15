package main

import (
	"flag"
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

	once := flag.Bool("once", false, "fetch one recipe, process it, publish to the channel, then exit (for testing)")
	flag.Parse()

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
	aiClient, err := ai.New(cfg.AIProvider, cfg.AIAPIKey(), cfg.AIModel, cfg.AIMaxTokens)
	if err != nil {
		log.Fatalf("ai client: %v", err)
	}
	log.Printf("[main] using AI provider %q", cfg.AIProvider)
	tgClient := telegram.New(cfg.TelegramToken, cfg.TelegramChat)

	// Build the two pipeline stages.
	fetcher := pipeline.NewFetcher(
		spoonClient, aiClient, store,
		string(prompt), cfg.ImageDir, cfg.FetchBatchSize,
	)
	publisher := pipeline.NewPublisher(store, tgClient)

	if *once {
		runOnce(spoonClient, fetcher, tgClient, cfg.ImageDir)
		return
	}

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

// runOnce fetches a single fresh recipe, processes it through the AI, and
// publishes it to the channel — then exits. Used for iterative testing of
// prompt changes against the real channel. Bypasses storage entirely so the
// same recipe can be sent again on the next run if needed.
func runOnce(spoon *spoonacular.Client, fetcher *pipeline.Fetcher, tg *telegram.Client, imageDir string) {
	log.Printf("[once] fetching one recipe from spoonacular")
	recipes, err := spoon.GetRandom(1)
	if err != nil {
		log.Fatalf("[once] spoonacular: %v", err)
	}
	if len(recipes) == 0 {
		log.Fatalf("[once] spoonacular returned no recipes")
	}
	r := recipes[0]
	log.Printf("[once] got %d %q", r.ID, r.Title)

	content, err := fetcher.ProcessRecipe(r)
	if err != nil {
		log.Fatalf("[once] process: %v", err)
	}

	imagePath, err := spoon.DownloadImage(r.Image, imageDir, r.ID)
	if err != nil {
		log.Printf("[once] image download failed: %v (publishing text-only)", err)
		imagePath = ""
	}

	if imagePath != "" {
		err = tg.Publish(imagePath, content)
	} else {
		err = tg.PublishText(content)
	}
	if err != nil {
		log.Fatalf("[once] publish: %v", err)
	}
	log.Printf("[once] published to channel ✓")
}
