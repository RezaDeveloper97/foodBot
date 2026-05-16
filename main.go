package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

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

	once := flag.Bool("once", false, "fetch one recipe, process it, publish to the channel, then exit (sync path)")
	migrate := flag.Bool("migrate", false, "create the MySQL database (if missing) and apply the schema, then exit")
	batchTest := flag.Bool("batch-test", false, "end-to-end smoke test: submit a 1-recipe batch, poll until done, save + publish, then exit")
	batchSubmit := flag.Bool("batch-submit", false, "submit one batch now and exit (use before sleep). Use -count to override size")
	batchCollect := flag.Bool("batch-collect", false, "poll in-progress batches once and ingest any results, then exit")
	batchStatus := flag.Bool("batch-status", false, "print a summary of recent batches and the publish queue, then exit")
	batchCount := flag.Int("count", 0, "override batch size (only meaningful with -batch-submit)")
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

	spoonClient := spoonacular.New(cfg.SpoonacularKey)
	aiClient, err := ai.New(cfg.AIProvider, cfg.AIAPIKey(), cfg.AIModel, cfg.AIMaxTokens, cfg.AITemperature)
	if err != nil {
		log.Fatalf("ai client: %v", err)
	}
	log.Printf("[main] using AI provider %q (temperature=%.2f, batch_mode=%v)",
		cfg.AIProvider, cfg.AITemperature, cfg.AIBatchMode)
	tgClient := telegram.New(cfg.TelegramToken, cfg.TelegramChat)

	// Sync pipeline stages (always built — used by -once and the non-batch path).
	fetcher := pipeline.NewFetcher(
		spoonClient, aiClient, store,
		string(prompt), cfg.ImageDir, cfg.FetchBatchSize,
	)
	publisher := pipeline.NewPublisher(store, tgClient)

	// Batch pipeline stages — only built when the provider supports batching.
	// Falling back to nil here means non-anthropic providers still work end to
	// end on the sync path; the batch CLI commands check for nil and abort.
	var submitter *pipeline.BatchSubmitter
	var collector *pipeline.BatchCollector
	if bp := ai.AsBatchProvider(aiClient); bp != nil {
		submitter = pipeline.NewBatchSubmitter(
			spoonClient, bp, store,
			string(prompt), cfg.ImageDir, cfg.AIProvider, cfg.FetchBatchSize,
		)
		collector = pipeline.NewBatchCollector(bp, aiClient, store, string(prompt))
	}

	// One-shot CLI commands — each exits when done.
	switch {
	case *batchStatus:
		printBatchStatus(store)
		return
	case *batchTest:
		mustHaveBatch(submitter, collector)
		runBatchTest(spoonClient, submitter, collector, publisher, store)
		return
	case *batchSubmit:
		mustHaveBatch(submitter, collector)
		id := submitter.RunSized(*batchCount)
		if id == "" {
			log.Fatalf("[batch-submit] no batch submitted")
		}
		log.Printf("[batch-submit] done. Batch id: %s — leave the daemon running (or rely on cron) to collect results.", id)
		return
	case *batchCollect:
		mustHaveBatch(submitter, collector)
		collector.Run()
		return
	case *once:
		runOnce(spoonClient, fetcher, publisher, store, cfg.ImageDir)
		return
	}

	// Daemon mode.
	startupKickoff(cfg, store, fetcher, submitter)

	c := cron.New()
	if cfg.AIBatchMode {
		// In batch mode the fetch cron submits a batch; results come in
		// asynchronously and the collector cron picks them up.
		if _, err := c.AddFunc(cfg.FetchCron, func() { submitter.Run() }); err != nil {
			log.Fatalf("schedule batch submitter (%q): %v", cfg.FetchCron, err)
		}
		if _, err := c.AddFunc(cfg.BatchPollCron, collector.Run); err != nil {
			log.Fatalf("schedule batch collector (%q): %v", cfg.BatchPollCron, err)
		}
		log.Printf("[main] BATCH MODE — submit %q, poll %q, publish %q",
			cfg.FetchCron, cfg.BatchPollCron, cfg.PublishCron)
	} else {
		if _, err := c.AddFunc(cfg.FetchCron, fetcher.Run); err != nil {
			log.Fatalf("schedule fetcher (%q): %v", cfg.FetchCron, err)
		}
		log.Printf("[main] SYNC MODE — fetch %q, publish %q", cfg.FetchCron, cfg.PublishCron)
	}
	if _, err := c.AddFunc(cfg.PublishCron, publisher.Run); err != nil {
		log.Fatalf("schedule publisher (%q): %v", cfg.PublishCron, err)
	}
	c.Start()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	log.Printf("[main] shutting down")
	<-c.Stop().Done()
	log.Printf("[main] stopped")
}

// startupKickoff makes sure the queue isn't empty when the daemon starts.
// In sync mode it runs the fetcher inline. In batch mode it submits a batch
// (if there's nothing queued AND nothing in-flight) so a fresh deploy doesn't
// idle until the next FetchCron tick.
func startupKickoff(
	cfg *config.Config,
	store *storage.Storage,
	fetcher *pipeline.Fetcher,
	submitter *pipeline.BatchSubmitter,
) {
	ready, err := store.CountReady()
	if err != nil {
		log.Fatalf("count ready: %v", err)
	}
	if ready > 0 {
		return
	}
	if cfg.AIBatchMode {
		queued, err := store.CountQueuedEntries()
		if err != nil {
			log.Fatalf("count queued: %v", err)
		}
		if queued > 0 {
			log.Printf("[main] queue empty but %d entries in-flight — collector will fill it", queued)
			return
		}
		log.Printf("[main] queue empty on startup — submitting one batch")
		submitter.Run()
		return
	}
	log.Printf("[main] queue empty on startup — running fetcher once")
	fetcher.Run()
}

func mustHaveBatch(submitter *pipeline.BatchSubmitter, collector *pipeline.BatchCollector) {
	if submitter == nil || collector == nil {
		log.Fatalf("batch commands require AI_PROVIDER=anthropic (current provider doesn't support batching)")
	}
}

// runBatchTest is the verifier the operator runs before going to bed: it
// proves every link in the batch chain (submit → poll → results → save →
// publish) works on this machine right now, with this config. Exits non-zero
// if anything fails.
func runBatchTest(
	spoon *spoonacular.Client,
	submitter *pipeline.BatchSubmitter,
	collector *pipeline.BatchCollector,
	publisher *pipeline.Publisher,
	store *storage.Storage,
) {
	log.Printf("[batch-test] step 1/5: fetching one fresh recipe from spoonacular")
	const maxAttempts = 5
	var picked spoonacular.Recipe
	found := false
	for attempt := 1; attempt <= maxAttempts && !found; attempt++ {
		recipes, err := spoon.GetRandom(1)
		if err != nil {
			log.Fatalf("[batch-test] spoonacular: %v", err)
		}
		if len(recipes) == 0 {
			log.Fatalf("[batch-test] spoonacular returned no recipes")
		}
		exists, err := store.ExistsAnywhere(recipes[0].ID)
		if err != nil {
			log.Fatalf("[batch-test] exists check: %v", err)
		}
		if exists {
			log.Printf("[batch-test] %d already known, retrying", recipes[0].ID)
			continue
		}
		picked = recipes[0]
		found = true
	}
	if !found {
		log.Fatalf("[batch-test] couldn't find a fresh recipe after %d attempts", maxAttempts)
	}
	log.Printf("[batch-test] picked %d %q", picked.ID, picked.Title)

	log.Printf("[batch-test] step 2/5: submitting 1-recipe batch to anthropic")
	batchID := submitter.SubmitOne(picked, "test")
	if batchID == "" {
		log.Fatalf("[batch-test] batch submission failed (see log above)")
	}
	log.Printf("[batch-test] submitted batch %s ✓", batchID)

	log.Printf("[batch-test] step 3/5: polling until batch ends (this can take a few minutes)")
	const maxWait = 30 * time.Minute
	const pollEvery = 15 * time.Second
	deadline := time.Now().Add(maxWait)
	for {
		done := collector.ProcessBatch(batchID)
		if done {
			break
		}
		if time.Now().After(deadline) {
			log.Fatalf("[batch-test] batch %s still in-progress after %s — giving up. The bot will still pick it up on its next poll cron.", batchID, maxWait)
		}
		time.Sleep(pollEvery)
	}
	log.Printf("[batch-test] batch %s ended and results ingested ✓", batchID)

	log.Printf("[batch-test] step 4/5: verifying recipe landed in the ready queue")
	ready, err := store.CountReady()
	if err != nil {
		log.Fatalf("[batch-test] count ready: %v", err)
	}
	if ready == 0 {
		log.Fatalf("[batch-test] batch ended but no ready recipes — the AI result probably failed validation. Check journalctl above.")
	}
	log.Printf("[batch-test] %d ready recipe(s) in queue ✓", ready)

	log.Printf("[batch-test] step 5/5: publishing one recipe to the telegram channel")
	publisher.Run()
	log.Printf("[batch-test] ────────────────────────────────────────")
	log.Printf("[batch-test] END-TO-END BATCH PATH WORKS ✓")
	log.Printf("[batch-test] You can now safely run:")
	log.Printf("[batch-test]   ./recipe-bot -batch-submit -count N    (one-shot, then daemon picks it up)")
	log.Printf("[batch-test] or just keep the daemon running with AI_BATCH_MODE=true.")
}

// printBatchStatus is the "is my overnight job actually working?" inspector.
// Prints the recent batches and the current publish queue depth — enough to
// confirm before bed that the previous step succeeded.
func printBatchStatus(store *storage.Storage) {
	batches, err := store.ListRecentBatches(10)
	if err != nil {
		log.Fatalf("[batch-status] list batches: %v", err)
	}
	ready, _ := store.CountReady()
	queued, _ := store.CountQueuedEntries()

	fmt.Printf("Publish queue:    %d ready, %d in-flight (waiting on AI)\n", ready, queued)
	fmt.Println()
	if len(batches) == 0 {
		fmt.Println("No batches submitted yet.")
		return
	}
	fmt.Printf("%-30s  %-12s  %-19s  %-19s  %s\n", "BATCH_ID", "STATUS", "SUBMITTED", "COMPLETED", "COUNTS")
	for _, b := range batches {
		completed := "-"
		if b.CompletedAt != nil {
			completed = b.CompletedAt.Format("2006-01-02 15:04:05")
		}
		fmt.Printf("%-30s  %-12s  %-19s  %-19s  req=%d ok=%d err=%d\n",
			truncate(b.BatchID, 30), b.Status,
			b.SubmittedAt.Format("2006-01-02 15:04:05"), completed,
			b.RequestCount, b.SucceededCount, b.ErroredCount)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
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
