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
	AnthropicKey   string
	TelegramToken  string
	TelegramChat   string // @channelusername or numeric chat id

	FetchCron   string // when to pull + process a new batch of recipes
	PublishCron string // when to publish one ready recipe to the channel

	FetchBatchSize int
	DBPath         string
	ImageDir       string
	PromptPath     string
	AIModel        string
	AIMaxTokens    int
}

// Load reads .env (if present) then the environment, validates required
// values, and returns the assembled Config.
func Load() (*Config, error) {
	loadDotEnv(".env")

	cfg := &Config{
		SpoonacularKey: os.Getenv("SPOONACULAR_API_KEY"),
		AnthropicKey:   os.Getenv("ANTHROPIC_API_KEY"),
		TelegramToken:  os.Getenv("TELEGRAM_BOT_TOKEN"),
		TelegramChat:   os.Getenv("TELEGRAM_CHANNEL_ID"),

		FetchCron:   env("FETCH_CRON", "0 3 * * 1"),    // 03:00 every Monday
		PublishCron: env("PUBLISH_CRON", "0 12,20 * * *"), // 12:00 and 20:00 daily

		FetchBatchSize: envInt("FETCH_BATCH_SIZE", 10),
		DBPath:         env("DB_PATH", "data/store.json"),
		ImageDir:       env("IMAGE_DIR", "data/images"),
		PromptPath:     env("PROMPT_PATH", "prompt.txt"),
		AIModel:        env("AI_MODEL", "claude-sonnet-4-5"),
		AIMaxTokens:    envInt("AI_MAX_TOKENS", 1500),
	}

	var missing []string
	if cfg.SpoonacularKey == "" {
		missing = append(missing, "SPOONACULAR_API_KEY")
	}
	if cfg.AnthropicKey == "" {
		missing = append(missing, "ANTHROPIC_API_KEY")
	}
	if cfg.TelegramToken == "" {
		missing = append(missing, "TELEGRAM_BOT_TOKEN")
	}
	if cfg.TelegramChat == "" {
		missing = append(missing, "TELEGRAM_CHANNEL_ID")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required env vars: %s", strings.Join(missing, ", "))
	}
	return cfg, nil
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
