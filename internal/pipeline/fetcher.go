package pipeline

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

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

// localizedRecipe is the structured JSON the AI returns. The Go side owns the
// final telegram formatting (emojis, layout, numbering) so that even a weak
// model can only mess up the localized text — never the structure.
type localizedRecipe struct {
	Title       string   `json:"title"`
	Intro       string   `json:"intro"`
	Ingredients []string `json:"ingredients"`
	Steps       []string `json:"steps"`
	Tip         string   `json:"tip"`
}

func toPersianDigits(s string) string {
	digits := []rune("۰۱۲۳۴۵۶۷۸۹")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(digits[r-'0'])
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// formatPost turns the AI's structured output plus the original meta fields
// into the final Persian telegram post.
func formatPost(loc localizedRecipe, readyMinutes, servings int) string {
	var b strings.Builder

	fmt.Fprintf(&b, "🔥 %s\n\n", strings.TrimSpace(loc.Title))

	if intro := strings.TrimSpace(loc.Intro); intro != "" {
		b.WriteString(intro)
		b.WriteString("\n\n")
	}

	if readyMinutes > 0 || servings > 0 {
		var meta []string
		if readyMinutes > 0 {
			meta = append(meta, "⏱️ "+toPersianDigits(fmt.Sprintf("%d", readyMinutes))+" دقیقه")
		}
		if servings > 0 {
			meta = append(meta, "👥 "+toPersianDigits(fmt.Sprintf("%d", servings))+" نفر")
		}
		b.WriteString(strings.Join(meta, "  |  "))
		b.WriteString("\n\n")
	}

	if len(loc.Ingredients) > 0 {
		b.WriteString("🍴 مواد لازم:\n")
		for _, ing := range loc.Ingredients {
			fmt.Fprintf(&b, "• %s\n", strings.TrimSpace(ing))
		}
		b.WriteString("\n")
	}

	if len(loc.Steps) > 0 {
		b.WriteString("👨‍🍳 مراحل پخت:\n")
		for i, step := range loc.Steps {
			num := toPersianDigits(fmt.Sprintf("%d", i+1))
			fmt.Fprintf(&b, "%s. %s\n", num, strings.TrimSpace(step))
		}
		b.WriteString("\n")
	}

	if tip := strings.TrimSpace(loc.Tip); tip != "" {
		b.WriteString("💡 نکته‌ی سرآشپز:\n")
		b.WriteString(tip)
		b.WriteString("\n")
	}

	return strings.TrimSpace(b.String())
}

// ProcessRecipe runs a single spoonacular recipe through the AI and returns
// the final formatted Persian post — without touching storage, downloading
// the image, or publishing. Used by Run() and by the -dry test mode.
func (f *Fetcher) ProcessRecipe(r spoonacular.Recipe) (string, error) {
	steps := r.Steps()
	if len(steps) == 0 || len(r.ExtendedIngredients) == 0 {
		return "", fmt.Errorf("incomplete recipe: missing steps or ingredients")
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
		return "", fmt.Errorf("marshal payload: %w", err)
	}

	raw, err := f.ai.Process(f.prompt, string(payloadJSON))
	if err != nil {
		return "", fmt.Errorf("ai process: %w", err)
	}

	var loc localizedRecipe
	if err := json.Unmarshal([]byte(ai.CleanOutput(raw)), &loc); err != nil {
		return "", fmt.Errorf("parse ai json: %w (raw: %.200q)", err, raw)
	}
	return formatPost(loc, r.ReadyInMinutes, r.Servings), nil
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

		content, err := f.ProcessRecipe(r)
		if err != nil {
			log.Printf("[fetcher] process %d: %v", r.ID, err)
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
