#!/usr/bin/env bash
# deploy.sh — بیلد و ری‌استارت ربات روی سرور
#
# استفاده:
#   ./deploy.sh           # روال عادی: pull → mod tidy → build → restart
#   ./deploy.sh --no-pull # بدون git pull (وقتی محلی تغییر دادی)
#   ./deploy.sh --force   # حتی اگر کامیت جدیدی نباشد، باز هم rebuild کن
#   ./deploy.sh --logs    # فقط لاگ زنده سرویس را نشان بده و خارج شو
#
# پکیج‌ها فقط زمانی دانلود می‌شوند که go.mod یا go.sum عوض شده باشد.
set -euo pipefail

cd "$(dirname "$0")"

SERVICE=recipe-bot
BINARY=/usr/local/bin/recipe-bot
ENV_DST=/etc/recipe-bot.env

PULL=1
FORCE=0
LOGS_ONLY=0

for arg in "$@"; do
  case "$arg" in
    --no-pull)  PULL=0 ;;
    --force)    FORCE=1 ;;
    --logs)     LOGS_ONLY=1 ;;
    -h|--help)
      sed -n '2,11p' "$0"; exit 0 ;;
    *)
      echo "❌ گزینه ناشناخته: $arg" >&2; exit 1 ;;
  esac
done

if [ "$LOGS_ONLY" -eq 1 ]; then
  exec sudo journalctl -u "$SERVICE" -f -n 100
fi

# هش وضعیت ماژول‌ها قبل از pull — برای تشخیص نیاز به دانلود مجدد
mod_hash_before=""
if [ -f go.sum ]; then
  mod_hash_before=$(sha256sum go.mod go.sum 2>/dev/null | sha256sum | awk '{print $1}')
fi

# هش کامیت قبلی — برای تشخیص اینکه چیزی pull شد یا نه
commit_before=$(git rev-parse HEAD 2>/dev/null || echo "")

if [ "$PULL" -eq 1 ]; then
  echo "📥 در حال گرفتن آخرین تغییرات..."
  git pull --ff-only
else
  echo "⏭️  از git pull رد شد (--no-pull)"
fi

commit_after=$(git rev-parse HEAD 2>/dev/null || echo "")

# اگر هیچ تغییری نبود و --force نزده، خروج زودهنگام
if [ "$FORCE" -ne 1 ] && [ "$PULL" -eq 1 ] && [ "$commit_before" = "$commit_after" ]; then
  echo "✨ چیز جدیدی نیست — کامیت روی $commit_after است."
  echo "ℹ️  برای rebuild اجباری از --force استفاده کن."
  echo
  sudo systemctl status "$SERVICE" --no-pager || true
  exit 0
fi

# هش جدید go.mod / go.sum
mod_hash_after=""
if [ -f go.sum ]; then
  mod_hash_after=$(sha256sum go.mod go.sum 2>/dev/null | sha256sum | awk '{print $1}')
fi

# go mod download فقط وقتی نیاز است
if [ "$mod_hash_before" != "$mod_hash_after" ] || [ ! -d "$(go env GOMODCACHE)/cache/download" ]; then
  echo "📦 go.mod یا go.sum تغییر کرده — دانلود پکیج‌ها..."
  go mod download
  go mod verify
else
  echo "✅ پکیج‌ها به‌روزند — نیازی به دانلود نیست."
fi

echo "🔨 در حال بیلد کردن..."
go build -o /tmp/recipe-bot .

echo "📦 جایگزینی فایل اجرایی..."
sudo mv /tmp/recipe-bot "$BINARY"

if [ -f .env ]; then
  if ! sudo cmp -s .env "$ENV_DST" 2>/dev/null; then
    echo "🔐 کپی .env به $ENV_DST..."
    sudo cp .env "$ENV_DST"
    sudo chmod 600 "$ENV_DST"
  else
    echo "🔐 .env بدون تغییر — رد شد."
  fi
else
  echo "⚠️  فایل .env پیدا نشد، از این مرحله رد شد."
fi

echo "🔄 ری‌استارت سرویس..."
sudo systemctl restart "$SERVICE"

sleep 1
echo "✅ وضعیت سرویس:"
sudo systemctl status "$SERVICE" --no-pager || true

echo
echo "💡 برای دیدن لاگ زنده: ./deploy.sh --logs"
