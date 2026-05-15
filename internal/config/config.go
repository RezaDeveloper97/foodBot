package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime settings, loaded from environment variables
// (optionally seeded from a .env file in the working directory).
type Config struct {
	SpoonacularKey string
	TelegramToken  string
	TelegramChat   string // @channelusername or numeric chat id

	// AIProvider selects which backend the fetcher talks to: "anthropic",
	// "gemini", or "groq". The matching *Key field must be set; the others
	// are ignored, so you can keep all three in .env and switch by changing
	// AI_PROVIDER alone.
	AIProvider   string
	AnthropicKey string
	GeminiKey    string
	GroqKey      string

	FetchCron   string // when to pull + process a new batch of recipes
	PublishCron string // when to publish one ready recipe to the channel

	FetchBatchSize int
	DBPath         string
	ImageDir       string
	PromptPath     string
	AIModel        string  // empty => provider-specific default
	AIMaxTokens    int
	AITemperature  float64 // lower => more deterministic, fewer invented words
}

// Load reads .env (if present) then the environment, validates required
// values, and returns the assembled Config.
func Load() (*Config, error) {
	loadDotEnv(".env")

	cfg := &Config{
		SpoonacularKey: os.Getenv("SPOONACULAR_API_KEY"),
		TelegramToken:  os.Getenv("TELEGRAM_BOT_TOKEN"),
		TelegramChat:   os.Getenv("TELEGRAM_CHANNEL_ID"),

		AIProvider:   strings.ToLower(env("AI_PROVIDER", "gemini")),
		AnthropicKey: os.Getenv("ANTHROPIC_API_KEY"),
		GeminiKey:    os.Getenv("GEMINI_API_KEY"),
		GroqKey:      os.Getenv("GROQ_API_KEY"),

		FetchCron:   env("FETCH_CRON", "0 3 * * 1"),       // 03:00 every Monday
		PublishCron: env("PUBLISH_CRON", "0 12,20 * * *"), // 12:00 and 20:00 daily

		FetchBatchSize: envInt("FETCH_BATCH_SIZE", 10),
		DBPath:         env("DB_PATH", "data/store.json"),
		ImageDir:       env("IMAGE_DIR", "data/images"),
		PromptPath:     env("PROMPT_PATH", "prompt.txt"),
		AIModel:        os.Getenv("AI_MODEL"), // empty -> provider default
		AIMaxTokens:    envInt("AI_MAX_TOKENS", 1500),
		AITemperature:  envFloat("AI_TEMPERATURE", 0.3),
	}

	var missing []string
	if cfg.SpoonacularKey == "" {
		missing = append(missing, "SPOONACULAR_API_KEY")
	}
	if cfg.TelegramToken == "" {
		missing = append(missing, "TELEGRAM_BOT_TOKEN")
	}
	if cfg.TelegramChat == "" {
		missing = append(missing, "TELEGRAM_CHANNEL_ID")
	}
	switch cfg.AIProvider {
	case "anthropic":
		if cfg.AnthropicKey == "" {
			missing = append(missing, "ANTHROPIC_API_KEY")
		}
	case "gemini":
		if cfg.GeminiKey == "" {
			missing = append(missing, "GEMINI_API_KEY")
		}
	case "groq":
		if cfg.GroqKey == "" {
			missing = append(missing, "GROQ_API_KEY")
		}
	default:
		return nil, fmt.Errorf("AI_PROVIDER=%q is not supported (want anthropic|gemini|groq)", cfg.AIProvider)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required env vars: %s", strings.Join(missing, ", "))
	}
	return cfg, nil
}

// AIAPIKey returns the API key for the currently selected provider.
func (c *Config) AIAPIKey() string {
	switch c.AIProvider {
	case "anthropic":
		return c.AnthropicKey
	case "gemini":
		return c.GeminiKey
	case "groq":
		return c.GroqKey
	}
	return ""
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envFloat(key string, fallback float64) float64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			return n
		}
	}
	return fallback
}

// loadDotEnv reads a simple KEY=VALUE file. Existing environment variables
// are never overwritten. A missing file is not an error.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, val)
		}
	}
}
