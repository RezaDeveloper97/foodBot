package pipeline

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"recipe-bot/internal/ai"
	"recipe-bot/internal/spoonacular"
	"recipe-bot/internal/storage"
)

// BatchCollector is the back half of batch mode. On each tick it polls every
// in-progress batch; when one ends it downloads the JSONL results, parses
// each line, runs sync refinement on entries that fail validation, and
// finally writes them to the main recipes table — at which point the
// publisher picks them up on its own cron.
//
// The collector is idempotent: an entry that's already "processed" or
// "failed" is skipped, so the cron can fire as often as you like without
// risking double-publishes.
type BatchCollector struct {
	batch  ai.BatchProvider
	ai     ai.Provider // for sync refinement when validation flags issues
	store  *storage.Storage
	prompt string
}

// NewBatchCollector wires the collector.
func NewBatchCollector(
	batchProvider ai.BatchProvider,
	aiClient ai.Provider,
	store *storage.Storage,
	prompt string,
) *BatchCollector {
	return &BatchCollector{
		batch:  batchProvider,
		ai:     aiClient,
		store:  store,
		prompt: prompt,
	}
}

// Run is the cron entry point. Polls every in-progress batch once and
// processes any that have ended.
func (c *BatchCollector) Run() {
	batches, err := c.store.ListInProgressBatches()
	if err != nil {
		log.Printf("[batch-collect] list in-progress: %v", err)
		return
	}
	if len(batches) == 0 {
		log.Printf("[batch-collect] no in-progress batches")
		return
	}
	log.Printf("[batch-collect] %d in-progress batch(es) to poll", len(batches))
	for _, b := range batches {
		c.processBatch(b.BatchID)
	}
}

// ProcessBatch polls a single batch and, if it's done, ingests results.
// Exported so verification (-batch-test) can drive collection manually.
// Returns true when the batch has reached a terminal state (ended/canceled/
// expired) — useful for the test path's wait loop.
func (c *BatchCollector) ProcessBatch(batchID string) bool {
	return c.processBatch(batchID)
}

func (c *BatchCollector) processBatch(batchID string) bool {
	status, err := c.batch.PollBatch(batchID)
	if err != nil {
		log.Printf("[batch-collect] poll %s: %v", batchID, err)
		return false
	}
	log.Printf("[batch-collect] %s: status=%s processing=%d succeeded=%d errored=%d",
		batchID, status.ProcessingStatus,
		status.Counts.Processing, status.Counts.Succeeded, status.Counts.Errored)

	// Always update poll metadata so the operator can see we're alive even
	// when the batch hasn't finished yet.
	var completed *time.Time
	if status.ProcessingStatus != storage.BatchStatusInProgress {
		now := time.Now()
		if status.EndedAt != nil {
			now = *status.EndedAt
		}
		completed = &now
	}
	if err := c.store.UpdateBatchPoll(
		batchID, status.ProcessingStatus,
		status.Counts.Succeeded, status.Counts.Errored,
		status.ResultsURL, completed,
	); err != nil {
		log.Printf("[batch-collect] update batch %s: %v", batchID, err)
	}

	if status.ProcessingStatus == storage.BatchStatusInProgress {
		return false
	}

	// Terminal states without usable results — just mark all queued entries
	// as failed and move on.
	if status.ResultsURL == "" {
		log.Printf("[batch-collect] %s ended without results_url (status=%s) — marking entries failed",
			batchID, status.ProcessingStatus)
		c.failAllQueued(batchID, fmt.Sprintf("batch %s: no results (%s)", batchID, status.ProcessingStatus))
		return true
	}

	results, err := c.batch.FetchBatchResults(status.ResultsURL)
	if err != nil {
		log.Printf("[batch-collect] fetch results %s: %v", batchID, err)
		return true
	}
	c.ingestResults(batchID, results)
	return true
}

func (c *BatchCollector) failAllQueued(batchID, reason string) {
	entries, err := c.store.GetQueuedEntries(batchID)
	if err != nil {
		log.Printf("[batch-collect] list queued for %s: %v", batchID, err)
		return
	}
	for _, e := range entries {
		if err := c.store.MarkEntryFailed(e.ID, reason); err != nil {
			log.Printf("[batch-collect] mark entry %d failed: %v", e.ID, err)
		}
	}
}

// ingestResults walks the per-line results, matches each to its queued entry
// by custom_id, and turns the successful ones into ready recipes. Errored,
// canceled, and expired results land in the entry's error_message and the
// entry is marked failed.
func (c *BatchCollector) ingestResults(batchID string, results []ai.BatchResultItem) {
	queued, err := c.store.GetQueuedEntries(batchID)
	if err != nil {
		log.Printf("[batch-collect] get queued for %s: %v", batchID, err)
		return
	}
	byCustomID := make(map[string]*storage.BatchEntry, len(queued))
	for _, e := range queued {
		byCustomID[e.CustomID] = e
	}

	var processed, failed int
	for _, res := range results {
		entry, ok := byCustomID[res.CustomID]
		if !ok {
			// Already processed in an earlier run (idempotent retry) or
			// completely unknown — either way nothing to do.
			continue
		}

		switch res.Kind {
		case ai.BatchResultSucceeded:
			if err := c.ingestOne(entry, res.Content); err != nil {
				log.Printf("[batch-collect] entry %d (%d): %v", entry.ID, entry.SpoonacularID, err)
				if markErr := c.store.MarkEntryFailed(entry.ID, err.Error()); markErr != nil {
					log.Printf("[batch-collect] mark entry %d failed: %v", entry.ID, markErr)
				}
				failed++
				continue
			}
			if err := c.store.MarkEntryProcessed(entry.ID); err != nil {
				log.Printf("[batch-collect] mark entry %d processed: %v", entry.ID, err)
			}
			processed++
			log.Printf("[batch-collect] ready: %d (entry %d)", entry.SpoonacularID, entry.ID)

		default: // errored / canceled / expired
			reason := res.Error
			if reason == "" {
				reason = string(res.Kind)
			}
			log.Printf("[batch-collect] entry %d (%d) %s: %s",
				entry.ID, entry.SpoonacularID, res.Kind, reason)
			if err := c.store.MarkEntryFailed(entry.ID, reason); err != nil {
				log.Printf("[batch-collect] mark entry %d failed: %v", entry.ID, err)
			}
			failed++
		}
	}
	log.Printf("[batch-collect] %s ingested: %d processed, %d failed", batchID, processed, failed)
}

// ingestOne handles a single succeeded result: parse JSON, validate (with
// optional sync refine), reconstitute the original spoonacular recipe from
// the stored snapshot, assemble the final storage.Recipe, and save.
func (c *BatchCollector) ingestOne(entry *storage.BatchEntry, rawText string) error {
	loc, err := finalizeAIOutput(entry.SpoonacularID, rawText, c.prompt, c.ai)
	if err != nil {
		return err
	}

	var r spoonacular.Recipe
	if err := json.Unmarshal([]byte(entry.SpoonacularJSON), &r); err != nil {
		return fmt.Errorf("decode stored spoonacular json: %w", err)
	}

	rec := assembleRecipe(r, loc, entry.ImagePath)
	if err := c.store.SaveReady(rec); err != nil {
		return fmt.Errorf("save ready: %w", err)
	}
	return nil
}
