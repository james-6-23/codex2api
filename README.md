<p align="center">
  <img src="assets/banner.svg" alt="Codex2API" width="100%">
</p>

<p align="center">
  <b>English</b> | <a href="README.zh-CN.md">中文</a>
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Go-1.26-00ADD8?style=for-the-badge&logo=go&logoColor=white" alt="Go">
  <img src="https://img.shields.io/badge/Gin-1.12-00ACD7?style=for-the-badge" alt="Gin">
  <img src="https://img.shields.io/badge/React-19-61DAFB?style=for-the-badge&logo=react&logoColor=111827" alt="React">
  <img src="https://img.shields.io/badge/Vite-8-646CFF?style=for-the-badge&logo=vite&logoColor=white" alt="Vite">
  <img src="https://img.shields.io/badge/DB-PostgreSQL%20%7C%20SQLite-4169E1?style=for-the-badge&logo=postgresql&logoColor=white" alt="Database">
  <img src="https://img.shields.io/badge/Cache-Redis%20%7C%20Memory-DC382D?style=for-the-badge&logo=redis&logoColor=white" alt="Cache">
  <img src="https://img.shields.io/badge/API-OpenAI%20%7C%20Anthropic-10A37F?style=for-the-badge" alt="API">
  <img src="https://img.shields.io/badge/Docker-Ready-2496ED?style=for-the-badge&logo=docker&logoColor=white" alt="Docker">
</p>

**Turn a Codex account pool into an observable, schedulable, operations-ready OpenAI / Anthropic compatible gateway.** Codex2API is not a thin forwarding proxy. It is a long-running Codex access hub: it exposes `/v1/chat/completions`, `/v1/responses`, `/v1/messages`, Images, and Models endpoints while managing Refresh Token / Access Token accounts, health scoring, dynamic concurrency, rate-limit recovery, usage tracking, and admin operations behind the scenes.

Run it as a full **PostgreSQL + Redis** production stack or as a single-container **SQLite + in-memory cache** deployment. Point Codex CLI, Claude Code, the OpenAI SDK, or any compatible client at one Base URL, then manage accounts, proxies, API keys, prompt filtering, image workflows, and runtime settings from the built-in dashboard.

<table>
<tr><td width="210"><b>One compatible gateway</b></td><td>OpenAI-style Chat Completions / Responses / Images, Anthropic Messages, prefixless compatibility routes, and native Codex Responses forwarding are all exposed through one service.</td></tr>
<tr><td><b>Account-pool scheduler</b></td><td>Selection is driven by account status, health tier, scheduler score, dynamic concurrency, cooldown recovery, and recent usage so unhealthy accounts are avoided automatically. Supports <code>round_robin</code> and <code>remaining_quota</code> modes, with per-account credit billing flags.</td></tr>
<tr><td><b>Visual admin console</b></td><td>The embedded React / Vite dashboard covers account import and testing, API keys, proxy pools, image studio (text-to-image + image-to-image), prompt filtering, usage analytics, operations, scheduler board, and system settings.</td></tr>
<tr><td><b>Two deployment shapes</b></td><td>Use PostgreSQL + Redis for production or SQLite + Memory for lightweight single-node deployments; Docker images, source builds, local development, and the interactive deploy script are ready to use. SQLite mode binds to <code>127.0.0.1</code> by default for security.</td></tr>
<tr><td><b>Billing and observability</b></td><td>Per-account 5h/7d windowed USD cost tracking, credit quota support, API key usage tracking, OAuth PKCE token acquisition, prompt filtering, and a usage dashboard with request logs and trend charts.</td></tr>
</table>

---

## Live Demo

- Demo URL: [https://codex2api-latest-vu8j.onrender.com](https://codex2api-latest-vu8j.onrender.com)
- Demo password: `codex2api`

> The demo is only for trying the admin dashboard and basic UI flows. Do not upload real Refresh Tokens, Access Tokens, API keys, or any other sensitive data.

---

## Screenshots

> Screenshots use demo data. The actual dashboard depends on your account pool, request logs, and runtime environment.

![CodexProxy Dashboard](docs/screenshots/dashboard.png)

<details>
<summary>More admin dashboard screenshots</summary>

| Accounts | Dashboard Trends |
| --- | --- |
| ![Accounts](docs/screenshots/accounts.png) | ![Dashboard Trends](docs/screenshots/dashboard-trends.png) |

| Image Studio | Prompt Filter |
| --- | --- |
| ![Image Studio](docs/screenshots/image-studio.png) | ![Prompt Filter](docs/screenshots/prompt-filter.png) |

| Operations | Usage |
| --- | --- |
| ![Operations](docs/screenshots/operations.png) | ![Usage](docs/screenshots/usage.png) |

| Usage Guide | API Reference |
| --- | --- |
| ![Usage Guide](docs/screenshots/guide.png) | ![API Reference](docs/screenshots/api-reference.png) |

</details>

---

## Sponsors

> Want to appear here? Open an issue on GitHub.

<table>
<tr>
<td width="180" align="center" valign="middle"><a href="https://www.fastaitoken.com/register"><img src="assets/fastaitoken-logo.jpg" width="90" alt="FastAIToken"></a></td>
<td valign="middle"><b><a href="https://www.fastaitoken.com/register">FastAIToken</a></b> is a developer-first AI API gateway providing unified access to leading models including OpenAI, Claude, and Gemini. Fully OpenAI-API compatible and works seamlessly with Claude Code, Codex, Gemini CLI, Cherry Studio, Cline, and Continue. With a 1:1 top-up ratio (¥1 = $1 API credit) and routes ranging from 0.02× OpenAI (limited time) to 1.2× Claude Max, plus a public status page and 24/7 human support. Enterprise-ready with invoice support and 99% SLA dedicated account pools.</td>
</tr>
</table>

<table>
<tr>
<td width="180" align="center" valign="middle"><a href="https://aixor.org/sign-up?aff=LaKs"><img src="assets/aixor-logo.jpg" width="160" alt="AiXor"></a></td>
<td valign="middle"><b><a href="https://aixor.org/sign-up?aff=LaKs">AiXor</a></b> provides cost-effective AI model API access with support for mainstream models including OpenAI, Claude, and Gemini. Top-up ratio of ¥0.2 = $1 credit, bringing per-call costs down to under 10% of official pricing. Plans start at ¥25/28 days; the Premium plan (¥129/28 days) includes about $4,752 in model credit (plans cover OpenAI models only), with high-concurrency support and 95%+ SLA stability.</td>
</tr>
</table>

<table>
<tr>
<td width="180" align="center" valign="middle"><a href="https://aihub.top/register?aff=42WZVXN9KS4S"><img src="assets/aihub-logo.jpg" width="160" alt="AIHub"></a></td>
<td valign="middle"><b><a href="https://aihub.top/register?aff=42WZVXN9KS4S">AIHub</a></b> is a high-availability AI model API relay platform for individual developers and enterprise teams. Supports Codex/Claude Code at ~1/10 official pricing. Register using the link and promo code <code>CODEX2API</code> to get $3 test credit. </td>
</tr>
</table>

<table>
<tr>
<td width="180" align="center" valign="middle"><a href="https://ai.centos.hk"><b>星辰·AI</b></a></td>
<td valign="middle"><b><a href="https://ai.centos.hk">星辰·AI</a></b> provides stable and high-speed relay services for Claude Code / Codex / Gemini, suitable for both individual developers and teams.</td>
</tr>
</table>

---

## Contents

- [Live Demo](#live-demo)
- [Screenshots](#screenshots)
- [Sponsors](#sponsors)
- [Quick Start](#quick-start)
- [Documentation](#documentation)
- [Upgrade and Local Development](#upgrade-and-local-development)
- [Configuration](#configuration)
- [Public API](#public-api)
  - [Token Upload and Account Management](#token-upload-and-account-management)
- [Admin Dashboard](#admin-dashboard)
- [Core Capabilities](#core-capabilities)
- [Project Structure](#project-structure)
- [Notes](#notes)
- [Community](#community)
- [Disclaimer and License](#disclaimer-and-license)
- [Star History](#star-history)
- [Links](#links)

---

## Quick Start

> For detailed deployment instructions, see [DEPLOYMENT.md](docs/DEPLOYMENT.md).

### Deployment Modes

| Mode | File | Use Case |
| --- | --- | --- |
| Docker image deployment | `docker-compose.yml` | Recommended for servers and test environments using the prebuilt image |
| Local source container build | `docker-compose.local.yml` | Full container verification after local source changes |
| SQLite lightweight deployment | `docker-compose.sqlite.yml` | Single-node deployment without PostgreSQL or Redis |
| SQLite local source build | `docker-compose.sqlite.local.yml` | Local source verification for the lightweight SQLite mode |
| Local development | `go run .` + `npm run dev` | Backend and frontend development |

### Commands

Standard image mode:

```bash
git clone https://github.com/james-6-23/codex2api.git
cd codex2api
cp .env.example .env
docker compose pull
docker compose up -d
docker compose logs -f codex2api
```

Standard local build mode:

```bash
cp .env.example .env
docker compose -f docker-compose.local.yml up -d --build
docker compose -f docker-compose.local.yml logs -f codex2api
```

SQLite image mode:

```bash
cp .env.sqlite.example .env
docker compose -f docker-compose.sqlite.yml pull
docker compose -f docker-compose.sqlite.yml up -d
docker compose -f docker-compose.sqlite.yml logs -f codex2api
```

SQLite local build mode:

```bash
cp .env.sqlite.example .env
docker compose -f docker-compose.sqlite.local.yml up -d --build
docker compose -f docker-compose.sqlite.local.yml logs -f codex2api
```

After startup:

- Admin dashboard: `http://localhost:8080/admin/`
- Health check: `http://localhost:8080/health`

Notes:

- Standard and SQLite modes both read `.env`.
- Before switching deployment modes, replace `.env` with the matching example file.
- The SQLite lightweight mode runs a single `codex2api` container and stores data at `/data/codex2api.db`.
- **SQLite compose files bind to `127.0.0.1` by default for security.** To expose the SQLite service on all interfaces, set `BIND_HOST=0.0.0.0` in `.env` or override the port binding in the compose file. The standard compose files bind to `0.0.0.0` by default.
- The image studio library is stored under `/data/images`; uploaded admin backgrounds are stored under `/data/backgrounds`; Docker configurations persist `/data`.
- `docker compose down` does not delete named volumes by default. Data is removed only by commands such as `docker compose down -v`, `docker volume rm`, or `docker volume prune`.

---

## Documentation

| Document | Description | Path |
| --- | --- | --- |
| [Chinese README](README.zh-CN.md) | Main Chinese project overview | `README.zh-CN.md` |
| [API Documentation](docs/API.md) | API endpoints, request and response examples, error codes | `docs/API.md` |
| [Deployment Guide](docs/DEPLOYMENT.md) | Deployment modes, upgrade guide, backup and restore | `docs/DEPLOYMENT.md` |
| [Configuration Guide](docs/CONFIGURATION.md) | Environment variables, system settings, configuration priority | `docs/CONFIGURATION.md` |
| [Architecture](docs/ARCHITECTURE.md) | System architecture, scheduling algorithm, storage design | `docs/ARCHITECTURE.md` |
| [Troubleshooting](docs/TROUBLESHOOTING.md) | Common issues, diagnostic scripts, fixes | `docs/TROUBLESHOOTING.md` |
| [Contributing](docs/CONTRIBUTING.md) | Development rules, PR workflow, code standards | `docs/CONTRIBUTING.md` |

---

## Upgrade and Local Development

Upgrade the standard image deployment:

```bash
git pull && docker compose pull && docker compose up -d && docker compose logs -f codex2api
```

Back up the database before upgrading:

```bash
docker exec codex2api-postgres pg_dump -U codex2api codex2api > backup_$(date +%Y%m%d_%H%M%S).sql
```

Restore from a backup if needed:

```bash
docker exec -i codex2api-postgres psql -U codex2api codex2api < backup_xxx.sql
```

Unless you explicitly need to recreate resources, avoid `docker compose down` during upgrades. `pull + up -d` keeps existing containers and named volumes.

### Local Development

Backend:

```bash
cp .env.example .env
cd frontend && npm ci && npm run build && cd ..
go run .
```

The frontend must be built before the first backend run because Go embeds `frontend/dist` through `go:embed`.

Frontend dev server:

```bash
cd frontend && npm ci && npm run dev
```

Vite proxies `/api` and `/health` to the backend. During development, open `http://localhost:5173/admin/`.

---

## Configuration

### Environment Variables

> For the full configuration reference, see [CONFIGURATION.md](docs/CONFIGURATION.md).

| Variable | Description |
| --- | --- |
| `CODEX_PORT` | HTTP port, default `8080` |
| `CODEX_MAX_REQUEST_BODY_SIZE_MB` | HTTP request body limit in MB, default `48` |
| `ADMIN_SECRET` | Admin dashboard secret. When set, `/admin` prompts for authentication |
| `DATABASE_DRIVER` | Database driver: `postgres` or `sqlite` |
| `DATABASE_PATH` | SQLite database file path, used when `DATABASE_DRIVER=sqlite` |
| `DATABASE_HOST` | PostgreSQL host |
| `DATABASE_PORT` | PostgreSQL port, default `5432` |
| `DATABASE_USER` | PostgreSQL user |
| `DATABASE_PASSWORD` | PostgreSQL password |
| `DATABASE_NAME` | PostgreSQL database name |
| `DATABASE_SSLMODE` | PostgreSQL SSL mode, default `disable` |
| `CACHE_DRIVER` | Cache driver: `redis` or `memory` |
| `REDIS_ADDR` | Redis address, for example `redis:6379`, `redis://default:pass@host:6379/0`, or `rediss://default:pass@host:6379/0` |
| `REDIS_USERNAME` | Optional Redis ACL username |
| `REDIS_PASSWORD` | Redis password |
| `REDIS_DB` | Redis database number |
| `REDIS_TLS` | Enable TLS for `host:port` Redis addresses |
| `REDIS_INSECURE_SKIP_VERIFY` | Skip Redis TLS certificate verification, default `false` |
| `TZ` | Timezone, for example `Asia/Shanghai` |

Cloud Redis providers such as Aiven and Upstash often require TLS. Prefer a `rediss://...` URL when your provider gives one.

The standard `.env.example` declares `DATABASE_DRIVER=postgres` and `CACHE_DRIVER=redis`. For the lightweight SQLite mode, use `.env.sqlite.example`.

### Runtime Settings

Runtime business settings are stored in the database `SystemSettings` table and can be updated from the admin settings page.

Examples include `MaxConcurrency`, `GlobalRPM`, `TestModel`, `TestContent`, `TestConcurrency`, `ProxyURL`, `PgMaxConns`, `RedisPoolSize`, `AdminSecret`, `SchedulerMode`, and auto-cleanup switches.

Default settings are written automatically on first startup.

### API Keys and Admin Secret

- Public API keys come from the database API Keys table. If no key is configured, `/v1/*` skips API key authentication.
- Admin Secret priority:
  - If `ADMIN_SECRET` is set in `.env`, the environment variable wins.
  - Otherwise, the database `AdminSecret` value is used.
  - After login, the frontend sends `X-Admin-Key` when calling `/api/admin/*`.

---

## Public API

| Endpoint | Description |
| --- | --- |
| `POST /v1/chat/completions` | Chat Completions style endpoint |
| `POST /v1/responses` | Responses style endpoint |
| `POST /v1/images/generations` | OpenAI Images generation endpoint |
| `POST /v1/images/edits` | OpenAI Images edit endpoint |
| `GET /v1/models` | List available models (includes gpt-5.5, gpt-5.4, gpt-5.4-mini, gpt-5.3-codex, gpt-image-2, etc.) |
| `GET /health` | Health check |

> **Pricing**: gpt-5.5 is billed at $5.00/M input and $30.00/M output (standard tier). Priority tier: $12.50/M input, $75.00/M output. Other models follow pricing rules in the billing engine.

See [API.md](docs/API.md) for full request formats, response formats, and error codes.

### Token Upload and Account Management

The following admin endpoints require the `X-Admin-Key` header.

#### Add Refresh Token Accounts

```bash
# Single account
curl -X POST http://localhost:8080/api/admin/accounts \
  -H "X-Admin-Key: your-admin-secret" \
  -H "Content-Type: application/json" \
  -d '{"name": "my-account", "refresh_token": "rt_xxxxxxxxxxxx"}'

# Batch import, newline separated, up to 100 tokens per request
curl -X POST http://localhost:8080/api/admin/accounts \
  -H "X-Admin-Key: your-admin-secret" \
  -H "Content-Type: application/json" \
  -d '{"name": "batch", "refresh_token": "rt_xxx1\nrt_xxx2\nrt_xxx3"}'
```

#### Add Access Token Accounts

```bash
# Single AT-only account
curl -X POST http://localhost:8080/api/admin/accounts/at \
  -H "X-Admin-Key: your-admin-secret" \
  -H "Content-Type: application/json" \
  -d '{"name": "my-at", "access_token": "eyJhbGciOiJSUzI1NiIs..."}'

# Batch import, newline separated
curl -X POST http://localhost:8080/api/admin/accounts/at \
  -H "X-Admin-Key: your-admin-secret" \
  -H "Content-Type: application/json" \
  -d '{"access_token": "eyJtoken1...\neyJtoken2...\neyJtoken3..."}'
```

#### File Import

```bash
# Import Refresh Tokens from TXT, one token per line
curl -X POST http://localhost:8080/api/admin/accounts/import \
  -H "X-Admin-Key: your-admin-secret" \
  -F "file=@tokens.txt" \
  -F "format=txt"

# Import Refresh Tokens from JSON
curl -X POST http://localhost:8080/api/admin/accounts/import \
  -H "X-Admin-Key: your-admin-secret" \
  -F "file=@credentials.json" \
  -F "format=json"

# Import Access Tokens from TXT, one token per line
curl -X POST http://localhost:8080/api/admin/accounts/import \
  -H "X-Admin-Key: your-admin-secret" \
  -F "file=@access_tokens.txt" \
  -F "format=at_txt"
```

Import endpoints deduplicate tokens automatically. Existing tokens are not inserted again.

#### OAuth PKCE Authorization

Codex2API supports acquiring Refresh Tokens through the OAuth PKCE flow, useful when manual token extraction is impractical:

```bash
# Step 1: Generate an authorization URL
curl -X POST http://localhost:8080/api/admin/oauth/generate-auth-url \
  -H "X-Admin-Key: your-admin-secret" \
  -H "Content-Type: application/json" \
  -d '{}'

# Step 2: Open the returned auth_url in a browser, complete authorization
# Step 3: Exchange the authorization code for a token (auto-creates account)
curl -X POST http://localhost:8080/api/admin/oauth/exchange-code \
  -H "X-Admin-Key: your-admin-secret" \
  -H "Content-Type: application/json" \
  -d '{"session_id": "...", "code": "...", "state": "..."}'
```

See [API.md](docs/API.md) for the full OAuth flow and all admin endpoints.

---

## Admin Dashboard

Open `/admin/` in a browser.

| Page | Path | Description |
| --- | --- | --- |
| Dashboard | `/admin/` | Overview metrics, request trends, latency trends, token breakdown, model ranking |
| Accounts | `/admin/accounts` | Import, test, batch actions, scheduler state |
| API Keys | `/admin/api-keys` | API key creation, inspection, deletion, and credential management |
| Proxies | `/admin/proxies` | Proxy pool management, account proxy assignment, connectivity checks |
| Image Studio | `/admin/images/studio` | Text-to-image, image-to-image, prompt templates, task history, server-side image library |
| Prompt Filter | `/admin/prompt-filter/overview` | Rules, hit logs, testing, and handling mode configuration |
| Usage | `/admin/usage` | Request logs, metric cards, charts, log cleanup |
| Operations | `/admin/ops` | Runtime monitoring and system overview |
| Scheduler Board | `/admin/ops/scheduler` | Scheduler health, penalties, and score breakdown |
| Settings | `/admin/settings` | Runtime parameters and admin secret settings |
| Usage Guide | `/admin/docs` | Codex CLI and Claude Code integration examples |
| API Reference | `/admin/api-reference` | OpenAI-style endpoints and admin API reference |

---

## Core Capabilities

### Positioning

Codex2API is not just a forwarding proxy. It is a long-running Codex gateway with a full admin dashboard:

- Exposes a unified OpenAI-style API surface.
- Maintains a Refresh Token account pool and Access Token lifecycle.
- Coordinates persistence and runtime state through PostgreSQL + Redis or SQLite + in-memory cache.
- Provides operational observability through the `/admin` dashboard.

### Request Flow

Public request flow:

```text
Client -> Gin RPM limiter -> proxy.Handler API key check -> auth.Store scheduler -> upstream request -> response + usage logging
```

Admin flow:

```text
Browser -> embedded /admin frontend -> /api/admin/* -> database / account pool / cache layer
```

### Scheduler

The scheduler lives in `auth.Store`. It evaluates availability, scheduler priority, health tier, dynamic concurrency, historical errors, and recent usage before selecting an account.

Runtime state:

- `Status`: `ready`, `cooldown`, `error`
- `HealthTier`: `healthy`, `warm`, `risky`, `banned`
- `SchedulerScore`: real-time scheduling score based on a baseline of 100
- `DynamicConcurrencyLimit`: concurrency limit adjusted by health tier
- `SchedulerPriority`: strict account priority; higher-priority accounts are considered before health tier, score, or current load

Selection strategy:

1. Filter unavailable accounts, including `error`, `banned`, cooldown accounts, and accounts without an Access Token.
2. Recompute health tier, scheduler score, and dynamic concurrency.
3. Exclude accounts that have reached their concurrency limit.
4. Prefer higher `SchedulerPriority`, then `healthy > warm > risky > banned`; within the same priority and tier, prefer higher score and lower concurrency.
5. Apply a 15% random shuffle to reduce hotspots and starvation.

When multiple end users share one downstream API key, send `X-Codex2API-Affinity-Key` with a stable user or conversation identifier. Codex2API hashes it for local account affinity only and never forwards it upstream.

Concurrency rules:

| Tier | Concurrency Limit |
| --- | --- |
| `healthy` | System `MaxConcurrency` |
| `warm` | Base concurrency / 2, at least 1 |
| `risky` | Fixed at 1 |
| `banned` | Fixed at 0, not schedulable |

The persistent upstream WebSocket pool is also capped by each account's current `DynamicConcurrencyLimit`, so connection reuse cannot grow beyond the account's effective concurrency.

Observability:

- `GET /api/admin/accounts` shows health tier, scheduler score, and penalty details.
- `GET /api/admin/ops/overview` shows runtime and connection pool state.
- `/admin/ops/scheduler` provides the scheduler board.

**Scheduler mode** (`scheduler_mode`, via Admin Settings):

| Mode | Behavior |
| --- | --- |
| `round_robin` (default) | Round-robin across available accounts per health tier, weighted by dispatch score |
| `remaining_quota` | Prioritizes accounts with lower usage percent; round-robin for ties |

**Credit accounts** (per-account flags):

When an account has a credit-based billing model instead of a usage-based Free/Pro plan, you can mark it so the scheduler skips usage-window penalties:

| Field | Type | Effect |
| --- | --- | --- |
| `credit_enabled` | bool | Mark account as credit-based billing |
| `credit_skip_usage_window` | bool | When true, skip 7d/5h usage-window penalties for this account |

**Windowed USD cost**: The accounts table displays per-account billed cost over two windows -- the past 5 hours and the past 7 days -- aligned with each account's usage reset boundaries. This shows actual spending per account rather than estimated token costs.

---

## Project Structure

```text
codex2api/
|- main.go                      # Application entrypoint
|- Dockerfile                   # Multi-stage image build
|- docker-compose.yml           # Image deployment template
|- docker-compose.local.yml     # Local source build template
|- .env.example                 # Environment variable example
|- admin/                       # Admin API
|- auth/                        # Account pool, scheduler, token management
|- cache/                       # Redis and cache wrappers
|- config/                      # Environment loading
|- database/                    # Database access layer
|- proxy/                       # Public proxy, forwarding, rate limiting
`- frontend/                    # React + Vite admin dashboard
   |- src/pages/                # Dashboard / Accounts / API Keys / Proxies / Images / Prompt Filter / Ops / Usage / Settings / Docs
   |- src/components/           # UI components
   |- src/locales/              # zh/en locales
   `- vite.config.js            # Vite config
```

---

## Notes

- `docker-compose.yml` pulls the GHCR image for deployment. `docker-compose.local.yml` uses `build: .` for local source builds.
- The frontend base path is fixed at `/admin/` for both local development and production.
- Before manually building the Go binary, run `npm run build` in `frontend/`.
- `.env` controls physical runtime settings such as port, database, and Redis. Business settings are stored in the database and managed from the admin dashboard.
- API keys are stored in the database and configured through the admin dashboard.

---

## Community

- QQ group: [Join the "codex2api" group chat](https://qun.qq.com/universal-share/share?ac=1&authKey=6vwawW4MeqdACT7PajnHlf2lLkjfuNXEMSos67l9FBiAJ8t%2BKeaXJXB0dgsnhFa1&busi_data=eyJncm91cENvZGUiOiI4MTY3Mzk4NDIiLCJ0b2tlbiI6ImU1YW1KR3dNaXZoUXZDUWpYTWVncmdmMXhQV1RwQ21tbEhkdjB5VW45aWVPSjhFM2grMkRHNGdhWnhEU29oS08iLCJ1aW4iOiIxMTYzNDc2OTQ5In0%3D&data=adSomD6r40Al25rBr8PocFCKumQR5oxi1kq5jXjXxeJ49Z5cj4QLzbNf6vfIQKWMORrJntrZtcoyQuHg2ksUeA&svctype=4&tempid=h5_group_info) (group ID: 816739842)
- Telegram group: [Join the Telegram group](https://t.me/+9hJAA3ZWQxxmMzE5)

Join the group to discuss deployment, usage, and development questions.

---

## Disclaimer and License

- This project is for learning, research, and technical discussion only.
- This project is released under the `MIT License`.
- The project provides no warranty for direct or indirect consequences. Production use is at your own risk.

---

## Star History

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="https://api.star-history.com/svg?repos=james-6-23/codex2api&type=Date&theme=dark" />
  <source media="(prefers-color-scheme: light)" srcset="https://api.star-history.com/svg?repos=james-6-23/codex2api&type=Date" />
  <img alt="Star History Chart" src="https://api.star-history.com/svg?repos=james-6-23/codex2api&type=Date" />
</picture>

---

## Links

- [LINUX DO](https://linux.do/)
