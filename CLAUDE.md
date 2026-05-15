# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A Telegram bot in Go that, on a schedule, pulls recipes from Spoonacular, localizes them into Persian via a pluggable AI provider (Gemini / Groq / Anthropic), downloads the image, and publishes them to a Telegram channel. See `README.md` (Persian) for user-facing docs.

## Build / run

```bash
cp .env.example .env          # fill in required keys
go mod tidy
go build -o recipe-bot .
./recipe-bot
```

Module path is `recipe-bot`; all packages live under `internal/<pkg>/`.

Required env vars (validated at startup, see `internal/config/config.go`): `SPOONACULAR_API_KEY`, `TELEGRAM_BOT_TOKEN`, `TELEGRAM_CHANNEL_ID`, plus the key matching `AI_PROVIDER` — one of `GEMINI_API_KEY`, `GROQ_API_KEY`, `ANTHROPIC_API_KEY`. Optional: `AI_PROVIDER` (default `gemini`), `AI_MODEL` (empty -> provider default), `AI_MAX_TOKENS`, `FETCH_CRON`, `PUBLISH_CRON`, `FETCH_BATCH_SIZE`, `DB_PATH`, `IMAGE_DIR`, `PROMPT_PATH`.

## Deploy

`./deploy.sh` on the server: `git pull` (skip with `--no-pull`) → `go mod download` only if `go.mod`/`go.sum` changed → `go build` → install binary to `/usr/local/bin/recipe-bot` → sync `.env` to `/etc/recipe-bot.env` → `systemctl restart recipe-bot`. Flags: `--force` rebuilds even with no new commits; `--logs` tails `journalctl -u recipe-bot`.

No test suite exists yet — `go test ./...` runs nothing.

## Architecture

Two cron-scheduled stages decoupled through a persistent queue:

- **Fetcher** (`internal/pipeline/fetcher.go`) — on `FETCH_CRON` (default weekly), pulls a batch from Spoonacular, sends each recipe through the configured `ai.Provider` with the system prompt from `prompt.txt`, downloads the image to `IMAGE_DIR`, and writes a `Recipe{Status: ready}` row to storage. Dedup is by Spoonacular recipe `id`.
- **Publisher** (`internal/pipeline/publisher.go`) — on `PUBLISH_CRON` (default twice daily), pops the oldest `ready` row and posts to Telegram. Image-download failure for a given recipe degrades that post to text-only rather than aborting the batch.

The two stages never share an in-memory channel — only the JSON store — so a slow/failed external API on publish day cannot block publishing of already-prepared content.

**AI layer** (`internal/ai/`) is a `Provider` interface with one method (`Process(systemPrompt, userContent) (string, error)`) and three implementations: `anthropic.go`, `gemini.go`, `groq.go`. `ai.New(provider, key, model, maxTokens)` is the factory dispatched from `main.go` based on `AI_PROVIDER`. To add a new provider, drop a file in `internal/ai/`, add a `newX` constructor, and a case to `New`.

**Storage** (`internal/storage/storage.go`) is a mutex-guarded JSON file at `DB_PATH` (default `data/store.json`) holding `map[int]*Recipe` keyed by Spoonacular id. Lifecycle: `ready → published | failed`. The interface is deliberately narrow so swapping in SQLite/Postgres later is a contained change.

**Startup behavior** (`main.go`): if `store.CountReady() == 0` on boot, the fetcher runs once synchronously before the cron scheduler starts, so the first scheduled publish has something to send.

**Prompt** is loaded from `prompt.txt` at startup and passed to the AI client as a string — edit it without recompiling, but you must restart the process for changes to take effect.

## External constraints worth knowing

- Spoonacular free tier has a small daily quota — keep `FETCH_BATCH_SIZE` modest.
- Telegram photo captions are capped at 1024 chars. The telegram client is expected to split into photo + separate full-text message when the localized content exceeds that.
- The bot must be a **channel admin** to post.
- `AI_MODEL` must be an exact model id for the chosen provider. Provider defaults (set when `AI_MODEL` is empty) live in `internal/ai/client.go`: `gemini-2.0-flash`, `llama-3.3-70b-versatile`, `claude-sonnet-4-5`.
- Gemini and Groq both have generous free tiers; Anthropic does not. Gemini/Groq endpoints are blocked from Iranian IPs — the deploy target must have a clean route or the bot needs to run behind a proxy.
