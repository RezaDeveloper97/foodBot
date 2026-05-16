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
	migrate := flag.Bool("migrate", false, "create the MySQL database (if missing) and apply the schema, then exit")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	if *migrate {
		log.Printf("[migrate] applying schema to %q on %s:%d", cfg.MySQLDatabase, cfg.MySQLHost, cfg.MySQLPort)
		if err := storage.Migrate(cfg.MySQLServerDSN(), cfg.MySQLDSN(), cfg.MySQLDatabase); err != nil {
			log.Fatalf("[migrate] %v", err)
		}
		log.Printf("[migrate] done ✓")
		return
	}

	prompt, err := os.ReadFile(cfg.PromptPath)
	if err != nil {
		log.Fatalf("read prompt file %q: %v", cfg.PromptPath, err)
	}

	store, err := storage.New(cfg.MySQLDSN())
	if err != nil {
		log.Fatalf("storage: %v (hint: run with -migrate first)", err)
	}
	defer store.Close()
	if err := os.MkdirAll(cfg.ImageDir, 0o755); err != nil {
		log.Fatalf("image dir: %v", err)
	}

	// Build the API clients.
	spoonClient := spoonacular.New(cfg.SpoonacularKey)
	aiClient, err := ai.New(cfg.AIProvider, cfg.AIAPIKey(), cfg.AIModel, cfg.AIMaxTokens, cfg.AITemperature)
	if err != nil {
		log.Fatalf("ai client: %v", err)
	}
	log.Printf("[main] using AI provider %q (temperature=%.2f)", cfg.AIProvider, cfg.AITemperature)
	tgClient := telegram.New(cfg.TelegramToken, cfg.TelegramChat)

	// Build the two pipeline stages.
	fetcher := pipeline.NewFetcher(
		spoonClient, aiClient, store,
		string(prompt), cfg.ImageDir, cfg.FetchBatchSize,
	)
	publisher := pipeline.NewPublisher(store, tgClient)

	if *once {
		runOnce(spoonClient, fetcher, publisher, store, cfg.ImageDir)
		return
	}

	// On a fresh start the queue is empty, so run the fetcher once up front —
	// otherwise the first scheduled publish would have nothing to send.
	ready, err := store.CountReady()
	if err != nil {
		log.Fatalf("count ready: %v", err)
	}
	if ready == 0 {
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

// runOnce fetches a single fresh recipe, processes it through the AI, persists
// it to storage as "ready", and then publishes the next ready item from the
// queue — so the publish path always reads from the DB, never from memory.
func runOnce(
	spoon *spoonacular.Client,
	fetcher *pipeline.Fetcher,
	publisher *pipeline.Publisher,
	store *storage.Storage,
	imageDir string,
) {
	log.Printf("[once] fetching one fresh recipe from spoonacular")

	// Pull recipes until we find one that isn't already in the store. The free
	// tier returns very few duplicates, but bail after a few tries instead of
	// looping forever.
	const maxAttempts = 5
	var r spoonacular.Recipe
	found := false
	for attempt := 1; attempt <= maxAttempts && !found; attempt++ {
		recipes, err := spoon.GetRandom(1)
		if err != nil {
			log.Fatalf("[once] spoonacular: %v", err)
		}
		if len(recipes) == 0 {
			log.Fatalf("[once] spoonacular returned no recipes")
		}
		exists, err := store.Exists(recipes[0].ID)
		if err != nil {
			log.Fatalf("[once] exists check: %v", err)
		}
		if exists {
			log.Printf("[once] %d %q already in store, retrying", recipes[0].ID, recipes[0].Title)
			continue
		}
		r = recipes[0]
		found = true
	}
	if !found {
		log.Fatalf("[once] could not find a fresh (non-duplicate) recipe after %d attempts", maxAttempts)
	}
	log.Printf("[once] got %d %q", r.ID, r.Title)

	result, err := fetcher.ProcessRecipe(r)
	if err != nil {
		log.Fatalf("[once] process: %v", err)
	}

	imagePath, err := spoon.DownloadImage(r.Image, imageDir, r.ID)
	if err != nil {
		log.Printf("[once] image download failed: %v (will publish text-only)", err)
		imagePath = ""
	}

	if err := fetcher.SaveProcessed(r, result, imagePath); err != nil {
		log.Fatalf("[once] save: %v", err)
	}
	log.Printf("[once] saved %d to store as ready ✓", r.ID)

	publisher.Run()
}
