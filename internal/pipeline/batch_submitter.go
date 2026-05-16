package pipeline

import (
	"encoding/json"
	"fmt"
	"log"

	"recipe-bot/internal/ai"
	"recipe-bot/internal/spoonacular"
	"recipe-bot/internal/storage"
)

// BatchSubmitter is the fetcher's batch-mode counterpart. It pulls a batch
// of recipes from Spoonacular, downloads their images right away, and ships
// the localization work off to Anthropic's Messages Batch API as a single
// job. Once submitted it writes one entry per recipe to recipe_batch_entries
// and returns — the actual results are picked up later by BatchCollector.
//
// We download images at submit time (not at collect time) for two reasons:
//   - It surfaces broken image URLs immediately, while we still have the
//     recipe in hand to skip it.
//   - The collector is a hot path that runs on a tight schedule; we want it
//     spending its time on AI results, not file I/O.
type BatchSubmitter struct {
	spoon     *spoonacular.Client
	batch     ai.BatchProvider
	store     *storage.Storage
	prompt    string
	imageDir  string
	batchSize int
	provider  string // for the recipe_batches.provider column
}

// NewBatchSubmitter wires the submitter. batchProvider must be non-nil —
// callers should feature-detect via ai.AsBatchProvider() before constructing.
func NewBatchSubmitter(
	spoon *spoonacular.Client,
	batchProvider ai.BatchProvider,
	store *storage.Storage,
	prompt, imageDir, providerName string,
	batchSize int,
) *BatchSubmitter {
	return &BatchSubmitter{
		spoon:     spoon,
		batch:     batchProvider,
		store:     store,
		prompt:    prompt,
		imageDir:  imageDir,
		batchSize: batchSize,
		provider:  providerName,
	}
}

// Run pulls a fresh batch from Spoonacular and submits it. Logs counts and
// the assigned batch_id so the operator can grep journalctl after starting
// the daemon. Returns the batch id (empty string on no-op / failure) so
// callers like -batch-test can chain a wait + collect.
func (s *BatchSubmitter) Run() string {
	log.Printf("[batch-submit] requesting %d recipes from spoonacular", s.batchSize)
	recipes, err := s.spoon.GetRandom(s.batchSize)
	if err != nil {
		log.Printf("[batch-submit] spoonacular error: %v", err)
		return ""
	}

	entries, err := s.prepareEntries(recipes, "")
	if err != nil {
		log.Printf("[batch-submit] prepare entries: %v", err)
		return ""
	}
	if len(entries) == 0 {
		log.Printf("[batch-submit] no fresh recipes to submit (all duplicates or unprocessable)")
		return ""
	}
	return s.submit(entries)
}

// RunSized fetches exactly n recipes (overrides the configured batch size for
// a single invocation). Used by -batch-submit -count N when the operator
// wants a one-off bigger batch before going to bed.
func (s *BatchSubmitter) RunSized(n int) string {
	if n <= 0 {
		n = s.batchSize
	}
	log.Printf("[batch-submit] requesting %d recipes from spoonacular", n)
	recipes, err := s.spoon.GetRandom(n)
	if err != nil {
		log.Printf("[batch-submit] spoonacular error: %v", err)
		return ""
	}
	entries, err := s.prepareEntries(recipes, "")
	if err != nil {
		log.Printf("[batch-submit] prepare entries: %v", err)
		return ""
	}
	if len(entries) == 0 {
		log.Printf("[batch-submit] no fresh recipes to submit")
		return ""
	}
	return s.submit(entries)
}

// SubmitOne is the verification helper: it builds a one-recipe batch for a
// specific spoonacular recipe (skipping the dedup check, since callers may
// want to re-test with a known-good recipe). Returns the batch id.
func (s *BatchSubmitter) SubmitOne(r spoonacular.Recipe, customIDPrefix string) string {
	entries, err := s.prepareEntries([]spoonacular.Recipe{r}, customIDPrefix)
	if err != nil {
		log.Printf("[batch-submit] prepare one: %v", err)
		return ""
	}
	if len(entries) == 0 {
		log.Printf("[batch-submit] recipe %d unprocessable or duplicate", r.ID)
		return ""
	}
	return s.submit(entries)
}

// prepareEntry holds the in-memory state for one recipe between
// spoonacular-fetch and batch-submit: the raw recipe so we can JSON-encode
// it for storage, the user-content payload for Anthropic, the custom id we
// generated, and the downloaded image path.
type prepareEntry struct {
	recipe       spoonacular.Recipe
	customID     string
	userContent  string
	imagePath    string
	spoonJSON    string
}

// prepareEntries runs the per-recipe work that has to happen before we can
// actually submit the batch: dedup, payload build, image download, JSON
// snapshot of the spoonacular recipe. Recipes that fail individual steps are
// skipped with a log line, never abort the batch.
func (s *BatchSubmitter) prepareEntries(recipes []spoonacular.Recipe, customIDPrefix string) ([]prepareEntry, error) {
	out := make([]prepareEntry, 0, len(recipes))
	for i, r := range recipes {
		exists, err := s.store.ExistsAnywhere(r.ID)
		if err != nil {
			return nil, fmt.Errorf("exists %d: %w", r.ID, err)
		}
		if exists {
			log.Printf("[batch-submit] skip duplicate: %d %q", r.ID, r.Title)
			continue
		}

		userContent, err := buildUserPayload(r)
		if err != nil {
			log.Printf("[batch-submit] skip %d %q: %v", r.ID, r.Title, err)
			continue
		}

		spoonJSON, err := json.Marshal(r)
		if err != nil {
			log.Printf("[batch-submit] skip %d marshal: %v", r.ID, err)
			continue
		}

		imagePath, err := s.spoon.DownloadImage(r.Image, s.imageDir, r.ID)
		if err != nil {
			log.Printf("[batch-submit] image %d failed (%v) — will publish text-only", r.ID, err)
			imagePath = ""
		}

		prefix := customIDPrefix
		if prefix == "" {
			prefix = "recipe"
		}
		out = append(out, prepareEntry{
			recipe:      r,
			customID:    fmt.Sprintf("%s-%d-%d", prefix, r.ID, i),
			userContent: userContent,
			imagePath:   imagePath,
			spoonJSON:   string(spoonJSON),
		})
	}
	return out, nil
}

// submit takes prepared entries, ships them to Anthropic, persists the batch
// + entries, and returns the assigned batch id. Errors are logged and
// translated to "" so callers can keep flowing.
func (s *BatchSubmitter) submit(entries []prepareEntry) string {
	items := make([]ai.BatchItem, 0, len(entries))
	for _, e := range entries {
		items = append(items, ai.BatchItem{
			CustomID:    e.customID,
			UserContent: e.userContent,
		})
	}
	log.Printf("[batch-submit] shipping %d requests to anthropic", len(items))
	status, err := s.batch.SubmitBatch(s.prompt, items)
	if err != nil {
		log.Printf("[batch-submit] anthropic submit failed: %v", err)
		return ""
	}
	log.Printf("[batch-submit] anthropic accepted batch %s (status=%s)", status.BatchID, status.ProcessingStatus)

	dbBatch := &storage.Batch{
		BatchID:      status.BatchID,
		Provider:     s.provider,
		Status:       storage.BatchStatusInProgress,
		RequestCount: len(entries),
	}
	dbEntries := make([]*storage.BatchEntry, 0, len(entries))
	for _, e := range entries {
		dbEntries = append(dbEntries, &storage.BatchEntry{
			BatchID:         status.BatchID,
			CustomID:        e.customID,
			SpoonacularID:   e.recipe.ID,
			SpoonacularJSON: e.spoonJSON,
			ImageURL:        e.recipe.Image,
			ImagePath:       e.imagePath,
		})
	}
	if err := s.store.CreateBatch(dbBatch, dbEntries); err != nil {
		// We've already submitted to Anthropic but failed to persist. The
		// batch will complete and cost money but we can't process its results
		// — log loudly with the id so the operator can recover manually.
		log.Printf("[batch-submit] FATAL: anthropic batch %s submitted but DB persist failed: %v",
			status.BatchID, err)
		return ""
	}
	log.Printf("[batch-submit] persisted batch %s with %d entries", status.BatchID, len(entries))
	return status.BatchID
}
