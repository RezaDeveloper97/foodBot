package pipeline

import (
	"encoding/json"
	"log"

	"recipe-bot/internal/ai"
	"recipe-bot/internal/spoonacular"
	"recipe-bot/internal/storage"
)

// Fetcher pulls a batch of recipes from Spoonacular, localizes each one with
// the AI model, downloads its image, and queues the result as "ready".
// It runs on a schedule (weekly by default) so the publish queue stays topped
// up — this decouples API availability from publish time.
type Fetcher struct {
	spoon     *spoonacular.Client
	ai        ai.Provider
	store     *storage.Storage
	prompt    string
	imageDir  string
	batchSize int
}

// NewFetcher wires the fetcher's dependencies.
func NewFetcher(
	spoon *spoonacular.Client,
	aiClient ai.Provider,
	store *storage.Storage,
	prompt, imageDir string,
	batchSize int,
) *Fetcher {
	return &Fetcher{
		spoon:     spoon,
		ai:        aiClient,
		store:     store,
		prompt:    prompt,
		imageDir:  imageDir,
		batchSize: batchSize,
	}
}

// recipePayload is the clean, structured JSON handed to the AI model — only
// the fields it needs, nothing else.
type recipePayload struct {
	Title          string   `json:"title"`
	ReadyInMinutes int      `json:"ready_in_minutes"`
	Servings       int      `json:"servings"`
	Ingredients    []string `json:"ingredients"`
	Steps          []string `json:"steps"`
}

// Run executes one fetch cycle. Errors on individual recipes are logged and
// skipped so one bad recipe never aborts the whole batch.
func (f *Fetcher) Run() {
	log.Printf("[fetcher] requesting %d recipes from spoonacular", f.batchSize)
	recipes, err := f.spoon.GetRandom(f.batchSize)
	if err != nil {
		log.Printf("[fetcher] error: %v", err)
		return
	}

	added := 0
	for _, r := range recipes {
		// De-duplication guard: skip anything we've already queued.
		if f.store.Exists(r.ID) {
			log.Printf("[fetcher] skip duplicate: %d %q", r.ID, r.Title)
			continue
		}

		steps := r.Steps()
		if len(steps) == 0 || len(r.ExtendedIngredients) == 0 {
			log.Printf("[fetcher] skip incomplete: %d %q", r.ID, r.Title)
			continue
		}

		ingredients := make([]string, 0, len(r.ExtendedIngredients))
		for _, ing := range r.ExtendedIngredients {
			ingredients = append(ingredients, ing.Original)
		}

		payload := recipePayload{
			Title:          r.Title,
			ReadyInMinutes: r.ReadyInMinutes,
			Servings:       r.Servings,
			Ingredients:    ingredients,
			Steps:          steps,
		}
		payloadJSON, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			log.Printf("[fetcher] marshal payload %d: %v", r.ID, err)
			continue
		}

		content, err := f.ai.Process(f.prompt, string(payloadJSON))
		if err != nil {
			log.Printf("[fetcher] ai process %d: %v", r.ID, err)
			continue
		}

		// A failed image download is not fatal — we just publish text-only.
		imagePath, err := f.spoon.DownloadImage(r.Image, f.imageDir, r.ID)
		if err != nil {
			log.Printf("[fetcher] image download %d: %v (continuing without image)", r.ID, err)
			imagePath = ""
		}

		rec := &storage.Recipe{
			ID:        r.ID,
			Title:     r.Title,
			Content:   content,
			ImagePath: imagePath,
		}
		if err := f.store.SaveReady(rec); err != nil {
			log.Printf("[fetcher] save %d: %v", r.ID, err)
			continue
		}
		added++
		log.Printf("[fetcher] ready: %d %q", r.ID, r.Title)
	}

	log.Printf("[fetcher] done — %d new recipes queued (%d total ready)", added, f.store.CountReady())
}
