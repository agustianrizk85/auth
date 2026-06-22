#!/usr/bin/env bash
# Deploy master auth service: tarik kode terbaru, build, jalankan/-ulang via PM2.
# Jalankan di server dari dalam folder repo: ./deploy.sh
set -euo pipefail
cd "$(dirname "$0")"

echo "==> git pull"
git pull --ff-only

echo "==> go build"
export PATH="$PATH:/usr/local/go/bin"
CGO_ENABLED=0 go build -trimpath -o auth-server ./cmd/server

# Muat env (port, Postgres DSN, bootstrap, path kunci) dari file di luar git.
set -a; [ -f /opt/apps/auth.env ] && . /opt/apps/auth.env; set +a

echo "==> (re)start PM2: auth-be"
pm2 restart auth-be --update-env 2>/dev/null || pm2 start ./auth-server --name auth-be --update-env
pm2 save
echo "==> selesai. status:"
pm2 status auth-be
