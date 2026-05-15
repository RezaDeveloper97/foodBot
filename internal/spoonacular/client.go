package spoonacular

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

const baseURL = "https://api.spoonacular.com"

// Client talks to the Spoonacular recipe API.
type Client struct {
	apiKey string
	http   *http.Client
}

// New builds a client with a sane request timeout.
func New(apiKey string) *Client {
	return &Client{
		apiKey: apiKey,
		http:   &http.Client{Timeout: 30 * time.Second},
	}
}

// Ingredient is one line of the ingredient list ("1 cup flour").
type Ingredient struct {
	Original string `json:"original"`
}

// Step is a single numbered instruction.
type Step struct {
	Number int    `json:"number"`
	Step   string `json:"step"`
}

type instructionGroup struct {
	Steps []Step `json:"steps"`
}

// Recipe is the subset of Spoonacular's response we actually use.
type Recipe struct {
	ID                   int                `json:"id"`
	Title                string             `json:"title"`
	Image                string             `json:"image"`
	ReadyInMinutes       int                `json:"readyInMinutes"`
	Servings             int                `json:"servings"`
	ExtendedIngredients  []Ingredient       `json:"extendedIngredients"`
	AnalyzedInstructions []instructionGroup `json:"analyzedInstructions"`
}

// Steps flattens the (possibly grouped) analyzed instructions into an
// ordered slice of plain strings.
func (r Recipe) Steps() []string {
	var out []string
	for _, g := range r.AnalyzedInstructions {
		for _, s := range g.Steps {
			out = append(out, s.Step)
		}
	}
	return out
}

type randomResponse struct {
	Recipes []Recipe `json:"recipes"`
}

// GetRandom fetches n random recipes. Note: each call costs API quota, so
// the caller should batch sensibly rather than calling this per-post.
func (c *Client) GetRandom(n int) ([]Recipe, error) {
	q := url.Values{}
	q.Set("apiKey", c.apiKey)
	q.Set("number", fmt.Sprintf("%d", n))
	endpoint := fmt.Sprintf("%s/recipes/random?%s", baseURL, q.Encode())

	resp, err := c.http.Get(endpoint)
	if err != nil {
		return nil, fmt.Errorf("spoonacular request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("spoonacular status %d: %s", resp.StatusCode, string(body))
	}

	var parsed randomResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode spoonacular response: %w", err)
	}
	return parsed.Recipes, nil
}

// DownloadImage saves a recipe image into dir and returns the local path.
// Images are stored locally so the channel does not depend on Spoonacular's
// CDN staying reachable at publish time.
func (c *Client) DownloadImage(imageURL, dir string, recipeID int) (string, error) {
	if imageURL == "" {
		return "", fmt.Errorf("empty image url")
	}
	resp, err := c.http.Get(imageURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("image download status %d", resp.StatusCode)
	}

	ext := filepath.Ext(imageURL)
	if ext == "" || len(ext) > 5 {
		ext = ".jpg"
	}
	path := filepath.Join(dir, fmt.Sprintf("%d%s", recipeID, ext))

	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		return "", err
	}
	return path, nil
}
