# run-local.ps1 — Jalankan auth pusat (PostgreSQL) secara lokal.
#
# Menanyakan password Postgres saat dijalankan (input tersembunyi, TIDAK disimpan
# ke file). Semua konfigurasi lain otomatis. DB "greenpark" harus sudah ada.
#
# Jalankan dari folder auth:
#   powershell -ExecutionPolicy Bypass -File run-local.ps1
#
$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot

# --- Password Postgres: input aman, tidak tampil, tidak ditulis ke file ---
$sec = Read-Host "Password Postgres (user 'postgres')" -AsSecureString
$bstr = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($sec)
$plain = [Runtime.InteropServices.Marshal]::PtrToStringAuto($bstr)
[Runtime.InteropServices.Marshal]::ZeroFreeBSTR($bstr)
if ([string]::IsNullOrEmpty($plain)) { Write-Error "Password kosong."; exit 1 }

# URL-encode agar karakter seperti @ ! : aman di connection string.
Add-Type -AssemblyName System.Web
$enc = [System.Web.HttpUtility]::UrlEncode($plain)

# --- Konfigurasi (dibaca config.Load dari environment) ---
$env:AUTH_PORT             = "8090"
$env:AUTH_ALLOW_ORIGIN     = "*"
$env:AUTH_DATABASE_URL     = "postgres://postgres:$enc@localhost:5432/greenpark?sslmode=disable"
$env:AUTH_ISSUER           = "greenpark-auth"
$env:AUTH_ACCESS_TTL       = "15m"
$env:AUTH_REFRESH_TTL      = "720h"
$env:AUTH_PRIVATE_KEY_PATH = "keys/ed25519_private.pem"
$env:AUTH_PUBLIC_KEY_PATH  = "keys/ed25519_public.pem"
$env:AUTH_BOOTSTRAP_USERNAME = "superadmin"
$env:AUTH_BOOTSTRAP_NAME     = "Super Admin"
$env:AUTH_BOOTSTRAP_PASSWORD = "superadmin123"   # ganti kalau mau; hanya dipakai saat tabel user kosong

Write-Host ""
Write-Host "Menjalankan auth pusat (PostgreSQL) di http://localhost:8090 ..." -ForegroundColor Green
Write-Host "(Ctrl+C untuk berhenti)"
go run ./cmd/server
