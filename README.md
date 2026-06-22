# Greenpark Master Auth (SSO)

Layanan autentikasi terpusat untuk **semua** departemen Greenpark (finance,
marketing, sales, sdm, perencanaan, teknik, legalpermit, dst). Satu pintu login,
satu sumber kebenaran untuk akun & role, dan token yang bisa diverifikasi sendiri
oleh tiap backend departemen tanpa memanggil balik auth tiap request.

## Kenapa desain ini

| Keputusan | Pilihan | Alasan |
|---|---|---|
| Bentuk | Service SSO berdiri sendiri | Hilangkan duplikasi auth di tiap departemen; satu sumber akun/role |
| Token | **JWT Ed25519 (asimetris)** | Auth pegang *private key*; departemen cukup *public key*. Bocornya satu departemen **tidak** bisa memalsukan token |
| Verifikasi | Lokal (stateless) di tiap departemen | Cepat & skalabel; auth bukan titik tunggal per request |
| Revokasi | Refresh token revocable + access token pendek (15 mnt) | Dapat manfaat stateless **dan** bisa cabut akses |
| Storage | PostgreSQL | Sumber identitas, butuh relasional & konsisten |
| Password | **bcrypt** (cost 12) | Memory-hard, standar produksi |

Access token berumur pendek (default 15 menit) dan ditandatangani Ed25519.
Refresh token berumur panjang (default 30 hari), disimpan **hanya sebagai hash
SHA-256** di DB, dan **dirotasi** tiap kali dipakai (token lama langsung
di-revoke). Mengubah password / menonaktifkan user / mengganti role otomatis
mencabut semua refresh token user tsb.

## Menjalankan

```bash
cp .env.example .env          # set AUTH_DATABASE_URL & AUTH_BOOTSTRAP_PASSWORD
go run ./cmd/server
```

Saat pertama jalan layanan akan:
1. Membuat pasangan kunci Ed25519 di `keys/` (private 0600, public 0644).
2. Menjalankan migrasi tabel (`auth_users`, `auth_memberships`, `auth_departments`, `auth_refresh_tokens`).
3. Menyemai daftar departemen.
4. Membuat superadmin pertama dari `AUTH_BOOTSTRAP_*` **bila tabel user masih kosong**.

> Hanya Postgres yang didukung — `AUTH_DATABASE_URL` wajib diisi.

## Model akses

- **Super user** (`super: true`) — admin penuh untuk *semua* departemen + boleh memakai API admin auth.
- Selain itu, user punya **role per-departemen**: `admin` (boleh tulis) atau `viewer` (baca saja).
- Token membawa klaim `super` dan `roles` (map `kode-departemen -> role`).

## Endpoint

### Publik
| Method | Path | Keterangan |
|---|---|---|
| GET | `/api/health` | Health check |
| GET | `/.well-known/jwks.json` | Public key (JWKS) untuk verifikasi token |
| POST | `/api/auth/login` | `{username, password}` → `{accessToken, refreshToken, user, ...}` |
| POST | `/api/auth/refresh` | `{refreshToken}` → pasangan token baru (rotasi) |
| POST | `/api/auth/logout` | `{refreshToken}` → cabut refresh token |

### Sesi (butuh `Authorization: Bearer <accessToken>`)
| Method | Path | Keterangan |
|---|---|---|
| GET | `/api/auth/me` | Data user saat ini (fresh dari DB) |
| POST | `/api/auth/logout-all` | Cabut semua sesi user (logout di semua perangkat) |

### Admin (butuh super user)
| Method | Path | Keterangan |
|---|---|---|
| GET | `/api/admin/departments` | Daftar departemen |
| GET | `/api/admin/users` | Daftar user |
| POST | `/api/admin/users` | Buat user `{username,password,name,email,super,active,roles}` |
| GET | `/api/admin/users/{id}` | Detail user |
| PUT | `/api/admin/users/{id}` | Update sebagian (field nil = tak diubah; `roles` mengganti seluruh set) |
| DELETE | `/api/admin/users/{id}` | Hapus user (tak bisa hapus diri sendiri) |
| PUT | `/api/admin/users/{id}/roles/{dept}` | Set role `{role:"admin"\|"viewer"}` di satu departemen |
| DELETE | `/api/admin/users/{id}/roles/{dept}` | Cabut akses departemen |

### Contoh

```bash
# login
curl -s localhost:8090/api/auth/login \
  -d '{"username":"superadmin","password":"..."}'

# pakai access token
curl -s localhost:8090/api/auth/me -H "Authorization: Bearer $ACCESS"

# buat user finance (admin) + sales (viewer)
curl -s localhost:8090/api/admin/users -H "Authorization: Bearer $ACCESS" -d '{
  "username":"budi","name":"Budi","password":"rahasia123",
  "roles":{"finance":"admin","sales":"viewer"}
}'
```

## Integrasi di backend departemen

Setiap departemen memverifikasi token **secara lokal** dengan public key auth.
Tersedia middleware siap-pakai tanpa dependensi eksternal di
[`integration/authmw/authmw.go`](integration/authmw/authmw.go) — salin file itu
ke modul departemen (sesuaikan path package), lalu:

```go
v, _ := authmw.New(authmw.Options{
    JWKSURL:    "http://auth-host:8090/.well-known/jwks.json",
    Department: "finance",          // kode departemen service ini
    Issuer:     "greenpark-auth",
})

mux.HandleFunc("GET /api/dashboard", v.RequireAuth(h.dashboard)) // semua role dept
mux.HandleFunc("POST /api/projects", v.RequireAdmin(h.save))     // admin dept / super

// di dalam handler:
claims := authmw.From(r.Context())   // claims.Subject, claims.Username, claims.Roles
```

`authmw` mengambil JWKS sekali, menyimpannya di cache, dan menarik ulang bila
melihat `kid` baru (mis. setelah rotasi kunci). Token EdDSA diverifikasi murni
dengan public key — tidak ada panggilan balik ke auth per request.

Migrasi backend lama (yang punya auth sendiri) cukup: ganti
`requireAuth`/`requireAdmin` lokal dengan milik `authmw`, lalu pindahkan akun ke
master auth. Endpoint bisnis & handler lainnya tidak berubah.

## Keamanan

- Algoritma token dikunci ke `EdDSA` saat verifikasi → tak ada celah `alg:none` / alg-confusion.
- Private key tak pernah keluar dari service; departemen hanya menerima public key.
- Password bcrypt; jalur "user tidak ditemukan" tetap menjalankan hash dummy untuk meredam timing enumeration.
- Refresh token disimpan sebagai hash, dirotasi, dan dibersihkan berkala (GC tiap jam).
- `keys/`, `*.pem`, dan `.env` di-ignore git — jangan pernah commit private key.
