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

// LocalizedRecipe is the structured JSON the AI returns. The Go side owns the
// final telegram formatting (emojis, layout, numbering) so that even a weak
// model can only mess up the localized text — never the structure.
type LocalizedRecipe struct {
	Title       string   `json:"title"`
	Intro       string   `json:"intro"`
	Ingredients []string `json:"ingredients"`
	Steps       []string `json:"steps"`
	Tip         string   `json:"tip"`
}

// ProcessResult bundles what ProcessRecipe returns: the final formatted post
// (what Telegram sees) and the localized fields broken out, so the fetcher
// can save them into their own tables alongside the formatted content.
type ProcessResult struct {
	Content   string
	Localized LocalizedRecipe
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
func formatPost(loc LocalizedRecipe, readyMinutes, servings int) string {
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
// the final formatted Persian post plus the localized structured data —
// without touching storage, downloading the image, or publishing.
func (f *Fetcher) ProcessRecipe(r spoonacular.Recipe) (*ProcessResult, error) {
	steps := r.Steps()
	if len(steps) == 0 || len(r.ExtendedIngredients) == 0 {
		return nil, fmt.Errorf("incomplete recipe: missing steps or ingredients")
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
		return nil, fmt.Errorf("marshal payload: %w", err)
	}

	loc, err := f.generateWithRetry(r.ID, string(payloadJSON))
	if err != nil {
		return nil, err
	}
	return &ProcessResult{
		Content:   formatPost(loc, r.ReadyInMinutes, r.Servings),
		Localized: loc,
	}, nil
}

// maxRefineAttempts caps how many times we ask the model to patch its own
// output when the validator flags Persian-quality issues. Two is a sweet
// spot — a single retry catches the common cases (a stray English word,
// one formal phrase), and a second retry catches issues introduced by the
// first patch. Beyond that, returns diminish fast.
const maxRefineAttempts = 2

// generateWithRetry calls the AI, parses its JSON, and if the validator
// finds Persian-quality issues asks the model to patch its own output.
// A refine that errors or returns invalid JSON is treated as best-effort
// — we keep whatever clean output we already had and move on, rather than
// failing the whole recipe.
func (f *Fetcher) generateWithRetry(recipeID int, userPayload string) (LocalizedRecipe, error) {
	raw, err := f.ai.Process(f.prompt, userPayload)
	if err != nil {
		return LocalizedRecipe{}, fmt.Errorf("ai process: %w", err)
	}
	clean := ai.CleanOutput(raw)

	var loc LocalizedRecipe
	if err := json.Unmarshal([]byte(clean), &loc); err != nil {
		return LocalizedRecipe{}, fmt.Errorf("parse ai json: %w (raw: %.200q)", err, raw)
	}

	for attempt := 1; attempt <= maxRefineAttempts; attempt++ {
		issues := validateLocalized(loc)
		if len(issues) == 0 {
			break
		}
		log.Printf("[fetcher] recipe %d: %d validation issue(s) on attempt %d, asking model to fix",
			recipeID, len(issues), attempt)

		refineRaw, refineErr := f.ai.Process(f.prompt, ai.FormatRetryRequest(clean, issues))
		if refineErr != nil {
			log.Printf("[fetcher] recipe %d: refine %d failed: %v (keeping prior output)",
				recipeID, attempt, refineErr)
			break
		}
		refineClean := ai.CleanOutput(refineRaw)
		var refined LocalizedRecipe
		if err := json.Unmarshal([]byte(refineClean), &refined); err != nil {
			log.Printf("[fetcher] recipe %d: refine %d returned invalid JSON, keeping prior output",
				recipeID, attempt)
			break
		}
		loc, clean = refined, refineClean
	}
	return loc, nil
}

func validateLocalized(loc LocalizedRecipe) []ai.ValidationIssue {
	texts := make([]string, 0, 3+len(loc.Ingredients)+len(loc.Steps))
	texts = append(texts, loc.Title, loc.Intro, loc.Tip)
	texts = append(texts, loc.Ingredients...)
	texts = append(texts, loc.Steps...)
	return ai.Validate(texts...)
}

// pairLines zips the original (english) and localized (persian) lists into a
// position-indexed slice. If the AI returned a different number of localized
// lines than originals, the shorter list wins and a "" fills the gap on the
// other side — better to record what we have than to drop the whole recipe.
func pairLines(originals, localized []string) (ings []storage.Ingredient) {
	n := len(originals)
	if len(localized) > n {
		n = len(localized)
	}
	for i := 0; i < n; i++ {
		var orig, loc string
		if i < len(originals) {
			orig = originals[i]
		}
		if i < len(localized) {
			loc = localized[i]
		}
		ings = append(ings, storage.Ingredient{
			Position:  i + 1,
			Original:  orig,
			Localized: loc,
		})
	}
	return ings
}

func pairSteps(originals, localized []string) (out []storage.Step) {
	for _, ing := range pairLines(originals, localized) {
		out = append(out, storage.Step{
			Position:  ing.Position,
			Original:  ing.Original,
			Localized: ing.Localized,
		})
	}
	return out
}

// SaveProcessed builds a storage.Recipe from a spoonacular recipe plus the
// AI-processed result (and resolved image path) and writes it to the store as
// "ready". Shared by the scheduled batch path and the -once test path so the
// DB is always the source of truth before anything is published.
func (f *Fetcher) SaveProcessed(r spoonacular.Recipe, result *ProcessResult, imagePath string) error {
	origIngredients := make([]string, 0, len(r.ExtendedIngredients))
	for _, ing := range r.ExtendedIngredients {
		origIngredients = append(origIngredients, ing.Original)
	}
	rec := &storage.Recipe{
		ID:             r.ID,
		OriginalTitle:  r.Title,
		Title:          result.Localized.Title,
		Intro:          result.Localized.Intro,
		Tip:            result.Localized.Tip,
		ReadyInMinutes: r.ReadyInMinutes,
		Servings:       r.Servings,
		ImageURL:       r.Image,
		ImagePath:      imagePath,
		Content:        result.Content,
		Ingredients:    pairLines(origIngredients, result.Localized.Ingredients),
		Steps:          pairSteps(r.Steps(), result.Localized.Steps),
	}
	return f.store.SaveReady(rec)
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
		exists, err := f.store.Exists(r.ID)
		if err != nil {
			log.Printf("[fetcher] exists check %d: %v", r.ID, err)
			continue
		}
		if exists {
			log.Printf("[fetcher] skip duplicate: %d %q", r.ID, r.Title)
			continue
		}

		result, err := f.ProcessRecipe(r)
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

		if err := f.SaveProcessed(r, result, imagePath); err != nil {
			log.Printf("[fetcher] save %d: %v", r.ID, err)
			continue
		}
		added++
		log.Printf("[fetcher] ready: %d %q", r.ID, r.Title)
	}

	if total, err := f.store.CountReady(); err != nil {
		log.Printf("[fetcher] done — %d new recipes queued (count-ready error: %v)", added, err)
	} else {
		log.Printf("[fetcher] done — %d new recipes queued (%d total ready)", added, total)
	}
}
