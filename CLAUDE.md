# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A Telegram bot in Go that, on a schedule, pulls recipes from Spoonacular, localizes them into Persian via the Anthropic Messages API, downloads the image, and publishes them to a Telegram channel. See `README.md` (Persian) for user-facing docs.

## Repo state caveat (important)

The codebase is **mid-reorganization** and currently does not compile as-is:

- `main.go` (`package main`) imports `recipe-bot/internal/{ai,config,pipeline,spoonacular,storage,telegram}`.
- The other `.go` files in the repo root each declare one of those packages (`package config`, `package storage`, `package spoonacular`, `package pipeline` — `fetcher.go` and `publisher.go`), but they are flat in the root, not under `internal/<pkg>/`.
- There is **no `go.mod`** yet; the module path `recipe-bot` is only referenced from `main.go` imports.
- `internal/ai/client.go` and `internal/telegram/client.go` exist only under `mnt/user-data/outputs/recipe-bot/internal/` (a staging area), not in the actual source tree.

Before any build will succeed you need to: `go mod init recipe-bot`, move each root `.go` file into `internal/<package>/`, and copy the `ai` and `telegram` packages from `mnt/user-data/outputs/recipe-bot/internal/` into `internal/`. Don't "fix" build errors by rewriting imports in `main.go` — the imports are the intended layout; the file locations are what's wrong.

## Build / run

```bash
cp .env.example .env          # fill in required keys
go mod init recipe-bot        # only once, if go.mod is missing
go mod tidy
go build -o recipe-bot .
./recipe-bot
```

Required env vars (validated at startup, see `config.go`): `SPOONACULAR_API_KEY`, `ANTHROPIC_API_KEY`, `TELEGRAM_BOT_TOKEN`, `TELEGRAM_CHANNEL_ID`. Optional: `FETCH_CRON`, `PUBLISH_CRON`, `FETCH_BATCH_SIZE`, `DB_PATH`, `IMAGE_DIR`, `PROMPT_PATH`, `AI_MODEL`, `AI_MAX_TOKENS`.

No test suite exists yet — `go test ./...` runs nothing.

## Architecture

Two cron-scheduled stages decoupled through a persistent queue:

- **Fetcher** (`fetcher.go`, `package pipeline`) — on `FETCH_CRON` (default weekly), pulls a batch from Spoonacular, sends each recipe to Anthropic with the system prompt from `prompt.txt`, downloads the image to `IMAGE_DIR`, and writes a `Recipe{Status: ready}` row to storage. Dedup is by Spoonacular recipe `id`.
- **Publisher** (`publisher.go`, `package pipeline`) — on `PUBLISH_CRON` (default twice daily), pops the oldest `ready` row and posts to Telegram. Image-download failure for a given recipe degrades that post to text-only rather than aborting the batch.

The two stages never share an in-memory channel — only the JSON store — so a slow/failed external API on publish day cannot block publishing of already-prepared content.

**Storage** (`storage.go`) is a mutex-guarded JSON file at `DB_PATH` (default `data/store.json`) holding `map[int]*Recipe` keyed by Spoonacular id. Lifecycle: `ready → published | failed`. The interface is deliberately narrow so swapping in SQLite/Postgres later is a contained change.

**Startup behavior** (`main.go`): if `store.CountReady() == 0` on boot, the fetcher runs once synchronously before the cron scheduler starts, so the first scheduled publish has something to send.

**Prompt** is loaded from `prompt.txt` at startup and passed to the AI client as a string — edit it without recompiling, but you must restart the process for changes to take effect.

## External constraints worth knowing

- Spoonacular free tier has a small daily quota — keep `FETCH_BATCH_SIZE` modest.
- Telegram photo captions are capped at 1024 chars. The telegram client is expected to split into photo + separate full-text message when the localized content exceeds that.
- The bot must be a **channel admin** to post.
- `AI_MODEL` must be an exact Anthropic model id (see `claude-api` skill / Anthropic docs); the default in `config.go` is `claude-sonnet-4-5`.
