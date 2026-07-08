#!/usr/bin/env bash
# Deploy master auth service (Postgres — cmd/server): tarik kode terbaru, build,
# jalankan/-ulang via PM2. Jalankan di server dari dalam folder repo: ./deploy.sh
#
# Prasyarat (sekali saja):
#   - /opt/apps/auth.env berisi minimal: AUTH_PORT, AUTH_DATABASE_URL,
#     AUTH_BOOTSTRAP_PASSWORD, OLLAMA_KEY_FILE (opsional).
#   - Database Postgres sudah dibuat:  GO111MODULE=on go run ./cmd/createdb
set -euo pipefail
cd "$(dirname "$0")"

echo "==> git pull"
git pull --ff-only

# Muat env (port, Postgres DSN, bootstrap, path kunci) dari file di luar git —
# di-source SEBELUM build agar createdb/runtime punya AUTH_DATABASE_URL.
set -a; [ -f /opt/apps/auth.env ] && . /opt/apps/auth.env; set +a

echo "==> go build (module mode, cmd/server = Postgres)"
export PATH="$PATH:/usr/local/go/bin"
export GO111MODULE=on
CGO_ENABLED=0 go build -trimpath -o auth-server ./cmd/server

echo "==> (re)start PM2: auth-be"
pm2 restart auth-be --update-env 2>/dev/null || pm2 start ./auth-server --name auth-be --update-env
pm2 save
echo "==> selesai. status:"
pm2 status auth-be
