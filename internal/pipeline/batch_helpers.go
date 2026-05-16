package pipeline

import (
	"encoding/json"
	"fmt"
	"log"

	"recipe-bot/internal/ai"
	"recipe-bot/internal/spoonacular"
	"recipe-bot/internal/storage"
)

// buildUserPayload turns a spoonacular recipe into the JSON string we hand to
// the AI as the user message. Shared between the sync fetcher and the batch
// submitter so both paths produce byte-identical model inputs — if a recipe
// works in -once it'll work in a batch.
func buildUserPayload(r spoonacular.Recipe) (string, error) {
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
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal payload: %w", err)
	}
	return string(b), nil
}

// finalizeAIOutput runs the same parse → validate → refine loop the sync
// fetcher uses, but starting from raw text the caller already obtained (in
// batch mode that text comes out of the results JSONL, not a fresh API call).
// The refine step uses sync Process() — refines are rare and we want them to
// resolve before SaveReady, not wait another batch cycle. A nil aiClient
// disables refinement entirely (used for verification/dry runs).
func finalizeAIOutput(
	recipeID int,
	rawText, systemPrompt string,
	aiClient ai.Provider,
) (LocalizedRecipe, error) {
	clean := ai.CleanOutput(rawText)
	var loc LocalizedRecipe
	if err := json.Unmarshal([]byte(clean), &loc); err != nil {
		return LocalizedRecipe{}, fmt.Errorf("parse ai json: %w (raw: %.200q)", err, rawText)
	}
	if aiClient == nil {
		return loc, nil
	}

	for attempt := 1; attempt <= maxRefineAttempts; attempt++ {
		issues := validateLocalized(loc)
		if len(issues) == 0 {
			break
		}
		log.Printf("[batch] recipe %d: %d validation issue(s) on attempt %d, asking model to fix",
			recipeID, len(issues), attempt)

		refineRaw, refineErr := aiClient.Process(systemPrompt, ai.FormatRetryRequest(clean, issues))
		if refineErr != nil {
			log.Printf("[batch] recipe %d: refine %d failed: %v (keeping prior output)",
				recipeID, attempt, refineErr)
			break
		}
		refineClean := ai.CleanOutput(refineRaw)
		var refined LocalizedRecipe
		if err := json.Unmarshal([]byte(refineClean), &refined); err != nil {
			log.Printf("[batch] recipe %d: refine %d returned invalid JSON, keeping prior output",
				recipeID, attempt)
			break
		}
		loc, clean = refined, refineClean
	}
	return loc, nil
}

// assembleRecipe builds the storage.Recipe row from the original spoonacular
// recipe + the localized output + a resolved image path. Mirrors what the
// sync fetcher does in SaveProcessed, but standalone so the batch collector
// can call it without going through Fetcher.
func assembleRecipe(r spoonacular.Recipe, loc LocalizedRecipe, imagePath string) *storage.Recipe {
	origIngredients := make([]string, 0, len(r.ExtendedIngredients))
	for _, ing := range r.ExtendedIngredients {
		origIngredients = append(origIngredients, ing.Original)
	}
	return &storage.Recipe{
		ID:             r.ID,
		OriginalTitle:  r.Title,
		Title:          loc.Title,
		Intro:          loc.Intro,
		Tip:            loc.Tip,
		ReadyInMinutes: r.ReadyInMinutes,
		Servings:       r.Servings,
		ImageURL:       r.Image,
		ImagePath:      imagePath,
		Content:        formatPost(loc, r.ReadyInMinutes, r.Servings),
		Ingredients:    pairLines(origIngredients, loc.Ingredients),
		Steps:          pairSteps(r.Steps(), loc.Steps),
	}
}
