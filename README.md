# Codex2API

Codex2API is a **Go + Gin + React/Vite**-based Codex reverse proxy and admin dashboard project that supports:

- Standard mode: **PostgreSQL + Redis**
- Lightweight mode: **SQLite + in-memory cache**

It exposes OpenAI-compatible APIs externally, while internally maintaining a complete set of capabilities built around a **Refresh Token account pool**, including scheduling, refreshing, testing, rate-limit recovery, usage monitoring, and admin management.

---

## Table of Contents

- [Quick Deployment](#quick-deployment)
- [Full Documentation](#full-documentation)
- [Environment Setup](#environment-setup)
- [External APIs](#external-apis)
  - [Token Upload and Account Management](#token-upload-and-account-management)
- [Admin Dashboard](#admin-dashboard)
- [Core Capabilities](#core-capabilities)
- [Project Structure](#project-structure)
- [Common Notes](#common-notes)
- [Disclaimer and License](#disclaimer-and-license)

---

## Quick Deployment

> For detailed deployment instructions, see: [DEPLOYMENT.md](docs/DEPLOYMENT.md)

### Deployment Modes Overview

| Mode | File | Use Case |
| --- | --- | --- |
| Docker image deployment | `docker-compose.yml` | **Recommended** for servers / test environments; directly pull prebuilt images |
| Local source container build | `docker-compose.local.yml` | Full container validation after making local code changes |
| SQLite lightweight deployment | `docker-compose.sqlite.yml` | Lightweight single-machine deployment without PostgreSQL / Redis |
| SQLite local source build | `docker-compose.sqlite.local.yml` | Validate SQLite lightweight mode after local code changes |
| Local development | `go run .` + `npm run dev` | Frontend-backend joint debugging and development |

### Deployment Commands Quick Reference

Standard image version:

```bash
git clone https://github.com/james-6-23/codex2api.git
cd codex2api
cp .env.example .env
docker compose pull
docker compose up -d
docker compose logs -f codex2api
```

Standard local build version:

```bash
cp .env.example .env
docker compose -f docker-compose.local.yml up -d --build
docker compose -f docker-compose.local.yml logs -f codex2api
```

SQLite image version:

```bash
cp .env.sqlite.example .env
docker compose -f docker-compose.sqlite.yml pull
docker compose -f docker-compose.sqlite.yml up -d
docker compose -f docker-compose.sqlite.yml logs -f codex2api
```

SQLite local build version:

```bash
cp .env.sqlite.example .env
docker compose -f docker-compose.sqlite.local.yml up -d --build
docker compose -f docker-compose.sqlite.local.yml logs -f codex2api
```

Additional notes:

- Both the standard and SQLite versions read from `.env`
- Before switching deployment modes, replace the current `.env` with the corresponding example file
- The standard image version uses the fixed project name `codex2api`, with fixed volumes `codex2api_pgdata` and `codex2api_redisdata`
- The standard local build version uses the fixed project name `codex2api-local`, with fixed volumes `codex2api-local_pgdata` and `codex2api-local_redisdata`
- The SQLite image version uses the fixed project name `codex2api-sqlite`, with fixed volume `codex2api-sqlite_sqlite-data`
- The SQLite local build version uses the fixed project name `codex2api-sqlite-local`, with fixed volume `codex2api-sqlite-local_sqlite-data-local`
- Standard container name: `codex2api`
- SQLite image container name: `codex2api-sqlite`
- SQLite local build container name: `codex2api-sqlite-local`
- The SQLite lightweight version starts only a single `codex2api` container, with data stored at `/data/codex2api.db`
- The image workspace gallery is stored by default in `/data/images`; both the standard and SQLite Docker configurations persist `/data`
- `docker compose down` does not remove named volumes by default; only `docker compose down -v`, `docker volume rm`, or `docker volume prune` will delete persistent data
- Data volumes are isolated across different deployment modes; if you see empty data after switching compose files, you likely switched to another volume set rather than losing the original data

After startup, access:

- Admin dashboard: `http://localhost:8080/admin/`
- Health check: `http://localhost:8080/health`

> For more deployment details, see: [DEPLOYMENT.md](docs/DEPLOYMENT.md)

---

## Full Documentation

| Document | Description | Path |
|------|------|------|
| [API Documentation](docs/API.md) | All API endpoints, request/response examples, and error code descriptions | `docs/API.md` |
| [Deployment Guide](docs/DEPLOYMENT.md) | Deployment modes, upgrade guide, backup and restore | `docs/DEPLOYMENT.md` |
| [Configuration Guide](docs/CONFIGURATION.md) | Environment variables, system settings, configuration priority | `docs/CONFIGURATION.md` |
| [Architecture Guide](docs/ARCHITECTURE.md) | System architecture, scheduling algorithms, storage design | `docs/ARCHITECTURE.md` |
| [Troubleshooting](docs/TROUBLESHOOTING.md) | Common issue diagnosis, diagnostic scripts, solutions | `docs/TROUBLESHOOTING.md` |
| [Contributing Guide](docs/CONTRIBUTING.md) | Development rules, PR workflow, code standards | `docs/CONTRIBUTING.md` |

---

## Environment Setup

```bash
git pull && docker compose pull && docker compose up -d && docker compose logs -f codex2api
```

> **⚠️ Important: Back up the database before upgrading!**
>
> ```bash
> docker exec codex2api-postgres pg_dump -U codex2api codex2api > backup_$(date +%Y%m%d_%H%M%S).sql
> ```
>
> If data becomes abnormal after upgrading, restore it with:
>
> ```bash
> docker exec -i codex2api-postgres psql -U codex2api codex2api < backup_xxx.sql
> ```

Unless necessary, it is not recommended to run `docker compose down` during upgrades; for standard upgrades, `pull + up -d` is enough to reuse the existing containers and named volumes.

### Local Development Mode

**Backend:**

```bash
cp .env.example .env
cd frontend && npm ci && npm run build && cd ..
go run .
```

> On first startup, you must build the frontend first because Go embeds `frontend/dist` using `go:embed`.

**Frontend development server:**

```bash
cd frontend && npm ci && npm run dev
```

Vite will automatically proxy `/api` and `/health` to the backend. During development, visit `http://localhost:5173/admin/`.

---

## Environment Setup

### `.env` Environment Variables

> For complete configuration details, see: [CONFIGURATION.md](docs/CONFIGURATION.md)

| Variable | Description |
| --- | --- |
| `CODEX_PORT` | HTTP port, default `8080` |
| `ADMIN_SECRET` | Admin dashboard login secret; if set, a password prompt appears when first visiting `/admin` |
| `DATABASE_DRIVER` | Database driver, supports `postgres` / `sqlite` |
| `DATABASE_PATH` | SQLite data file path; effective when `DATABASE_DRIVER=sqlite` |
| `DATABASE_HOST` | PostgreSQL host; effective when `DATABASE_DRIVER=postgres` |
| `DATABASE_PORT` | PostgreSQL port, default `5432` |
| `DATABASE_USER` | PostgreSQL username |
| `DATABASE_PASSWORD` | PostgreSQL password |
| `DATABASE_NAME` | PostgreSQL database name |
| `DATABASE_SSLMODE` | PostgreSQL SSL mode, default `disable` |
| `CACHE_DRIVER` | Cache driver, supports `redis` / `memory` |
| `REDIS_ADDR` | Redis address, for example `redis:6379`, `redis://default:pass@host:6379/0`, or `rediss://default:pass@host:6379/0`; effective when `CACHE_DRIVER=redis` |
| `REDIS_USERNAME` | Optional Redis ACL username; can be omitted if the URL already includes a username |
| `REDIS_PASSWORD` | Redis password |
| `REDIS_DB` | Redis DB index |
| `REDIS_TLS` | Whether to enable TLS for Redis when using `host:port`; automatically enabled when using `rediss://` |
| `REDIS_INSECURE_SKIP_VERIFY` | Skip Redis TLS certificate verification, default `false`; only for self-signed certificates or troubleshooting |
| `TZ` | Time zone, for example `Asia/Shanghai` |

> Cloud Redis services such as Aiven and Upstash usually require TLS. It is recommended to set `REDIS_ADDR` directly to the platform-provided `rediss://...` URL. If you only provide `host:port`, also set `REDIS_TLS=true`.

The standard `.env.example` explicitly declares `DATABASE_DRIVER=postgres` and `CACHE_DRIVER=redis`; for SQLite lightweight mode, use `.env.sqlite.example` instead.

### Business Runtime Configuration

The following parameters are **stored in the `SystemSettings` table in the database** and can be modified from the settings page in the admin dashboard:

`MaxConcurrency`, `GlobalRPM`, `TestModel`, `TestConcurrency`, `ProxyURL`, `PgMaxConns`, `RedisPoolSize`, `AdminSecret`, automatic cleanup switches, and more.

On first startup, the program automatically writes default settings.

### API Key and Admin Secret

- **External API Key**: Determined by the API Keys stored in the database. If no Key is configured, `/v1/*` skips authentication.
- **Admin Dashboard Secret**:
  - If `ADMIN_SECRET` is set in `.env`, the environment variable takes precedence.
  - If `ADMIN_SECRET` is not set, it falls back to `AdminSecret` in the database.
  - When authentication is enabled, visiting `/admin` for the first time shows a password prompt; after frontend login succeeds, `/api/admin/*` is accessed through the `X-Admin-Key` request header.

---

## External APIs

| Endpoint | Description |
| --- | --- |
| `POST /v1/chat/completions` | Chat Completions-style endpoint |
| `POST /v1/responses` | Responses-style endpoint |
| `POST /v1/images/generations` | OpenAI Images generation endpoint |
| `POST /v1/images/edits` | OpenAI Images editing endpoint |
| `GET /v1/models` | Returns the list of available models |
| `GET /health` | Health check |

> For complete request/response formats and error codes, see the [API documentation](docs/API.md).

### Token Upload and Account Management

The following endpoints require the `X-Admin-Key` authentication header.

#### Add a Refresh Token account

```bash
# Add one
curl -X POST http://localhost:8080/api/admin/accounts \
  -H "X-Admin-Key: your-admin-secret" \
  -H "Content-Type: application/json" \
  -d '{"name": "my-account", "refresh_token": "rt_xxxxxxxxxxxx"}'

# Batch add (newline-separated, maximum 100 per request)
curl -X POST http://localhost:8080/api/admin/accounts \
  -H "X-Admin-Key: your-admin-secret" \
  -H "Content-Type: application/json" \
  -d '{"name": "batch", "refresh_token": "rt_xxx1\nrt_xxx2\nrt_xxx3"}'
```

#### Add an Access Token account (AT-only)

```bash
# Add one
curl -X POST http://localhost:8080/api/admin/accounts/at \
  -H "X-Admin-Key: your-admin-secret" \
  -H "Content-Type: application/json" \
  -d '{"name": "my-at", "access_token": "eyJhbGciOiJSUzI1NiIs..."}'

# Batch add (newline-separated)
curl -X POST http://localhost:8080/api/admin/accounts/at \
  -H "X-Admin-Key: your-admin-secret" \
  -H "Content-Type: application/json" \
  -d '{"access_token": "eyJtoken1...\neyJtoken2...\neyJtoken3..."}'
```

#### Bulk import from files

```bash
# Import Refresh Tokens (TXT, one per line)
curl -X POST http://localhost:8080/api/admin/accounts/import \
  -H "X-Admin-Key: your-admin-secret" \
  -F "file=@tokens.txt" \
  -F "format=txt"

# Import Refresh Tokens (JSON format)
curl -X POST http://localhost:8080/api/admin/accounts/import \
  -H "X-Admin-Key: your-admin-secret" \
  -F "file=@credentials.json" \
  -F "format=json"

# Import Access Tokens (AT-TXT, one per line)
curl -X POST http://localhost:8080/api/admin/accounts/import \
  -H "X-Admin-Key: your-admin-secret" \
  -F "file=@access_tokens.txt" \
  -F "format=at_txt"
```

> All import endpoints deduplicate automatically; existing Tokens are not inserted again. For more management endpoints (export, migration, OAuth authorization, etc.), see the [API documentation](docs/API.md).

---

## Admin Dashboard

Visit `/admin/` in your browser. It provides the following pages:

| Page | Path | Function |
| --- | --- | --- |
| Dashboard | `/admin/` | Overview metrics, request trends, latency trends, token distribution, model rankings |
| Account Management | `/admin/accounts` | Import, test, batch operations, scheduling info viewing |
| Usage Statistics | `/admin/usage` | Request logs, stat cards, charts, log clearing |
| Operations Overview | `/admin/ops` | Runtime monitoring and system overview |
| Scheduler Board | `/admin/ops/scheduler` | Scheduler health, penalty items, and score breakdown |
| System Settings | `/admin/settings` | Business runtime parameters and admin secret configuration |

---

## Core Capabilities

### Project Positioning

This project is not just a simple forwarding layer. It is a long-running Codex gateway and admin dashboard system:

- Exposes a unified OpenAI-style endpoint externally, hiding upstream multi-account differences
- Internally maintains a `Refresh Token`-based account pool, `Access Token` lifecycle handling, and runtime scheduling
- Uses PostgreSQL + Redis or SQLite + in-memory cache for configuration persistence and runtime coordination
- Provides comprehensive operational observability through the `/admin` dashboard

### Architecture Overview

**External request path:** client request → Gin RPM rate limiting → `proxy.Handler` API Key validation → `auth.Store` account selection → upstream request → response return + usage writeback

**Admin dashboard path:** browser → `/admin/` embedded frontend → `/api/admin/*` management APIs → database / account pool / cache layer

### Scheduling System

The scheduling core is located in `auth.Store`, which combines account availability, health status, dynamic concurrency, historical errors, and recent usage into account selection.

**Runtime state model:**

- `Status`: `ready` / `cooldown` / `error`
- `HealthTier`: `healthy` / `warm` / `risky` / `banned`
- `SchedulerScore`: real-time scheduling score with a baseline of 100
- `DynamicConcurrencyLimit`: concurrency cap dynamically reduced by health tier

**Account selection strategy:**

1. Filter out unavailable accounts (`error` / `banned` / cooling down / no AccessToken)
2. Recalculate health tier, scheduling score, and dynamic concurrency
3. Exclude accounts that already reached their concurrency limit
4. Sort by `healthy > warm > risky > banned`; within the same tier, prefer better scheduling score and lower concurrency
5. Apply 15% random shuffling to reduce hotspots and starvation

**Dynamic concurrency rules:**

| Tier | Concurrency Limit |
| --- | --- |
| `healthy` | System `MaxConcurrency` |
| `warm` | Base concurrency ÷ 2 (minimum 1) |
| `risky` | Fixed at 1 |
| `banned` | Fixed at 0; excluded from scheduling |

**Scheduling score penalties/rewards:**

| Signal | Impact |
| --- | --- |
| `unauthorized` | `-50`, linearly decays over 24h |
| `rate_limited` | `-22`, linearly decays over 1h |
| `timeout` | `-18`, linearly decays over 15min |
| `server error` | `-12`, linearly decays over 15min |
| Consecutive failures | `-6` each time, up to `-24` |
| Consecutive successes | `+2` each time, up to `+12` |
| Recent success rate too low | `<75%` deduct 8, `<50%` deduct 15 |
| Free 7d usage | `≥70%` deduct 8 → `≥100%` deduct 40 |
| Latency EWMA | `≥5s` deduct 4 → `≥20s` deduct 15 |

**Cooldown and recovery mechanisms:**

- **429**: Prefer parsing upstream `resets_at`; otherwise infer cooldown time by plan type
- **401**: Immediately enters `banned`, cools down for 6h, escalates to 24h if triggered again within 24h
- Cooldown state is persisted to PostgreSQL and automatically restored after restart
- The backend periodically performs low-frequency recovery probes for `banned` accounts

**Scheduling observability:**

- `GET /api/admin/accounts` — health tier, scheduling score, penalty breakdown
- `GET /api/admin/ops/overview` — system runtime state and connection pool overview
- `/admin/ops/scheduler` — frontend scheduler board

---

## Project Structure

```text
codex2api/
├─ main.go                      # Program entry
├─ Dockerfile                   # Multi-stage image build
├─ docker-compose.yml           # Image deployment template
├─ docker-compose.local.yml     # Local source build template
├─ .env.example                 # Environment variable example
├─ admin/                       # Admin dashboard API
├─ auth/                        # Account pool, scheduling, and token management
├─ cache/                       # Redis cache wrapper
├─ config/                      # Environment variable loading
├─ database/                    # PostgreSQL access layer
├─ proxy/                       # External proxy, forwarding, and rate limiting
└─ frontend/                    # React + Vite admin dashboard
   ├─ src/pages/                # Dashboard / Accounts / Usage / Ops / Settings
   ├─ src/components/           # UI components
   ├─ src/locales/              # i18n language files (zh/en)
   └─ vite.config.js            # Vite configuration
```

---

## Common Notes

- `docker-compose.yml` pulls the GHCR image for deployment; `docker-compose.local.yml` uses `build: .` for local builds
- The frontend base path is fixed at `/admin/`, consistent between local development and production deployment
- Before manually building the Go binary locally, you must first run `npm run build` in `frontend/`
- `.env` only handles infrastructure-level configuration such as port, database, and Redis; business parameters are maintained in the admin dashboard database
- API Keys are determined by the database and configured in the admin dashboard

---

## Disclaimer and License

- This project is for learning, research, and technical communication only.
- This project is open-sourced under the `MIT License`.
- The project provides no warranty for any direct or indirect consequences of use; users assume all risks of production use.

---

## Friendly Links

- [LINUX DO](https://linux.do/)