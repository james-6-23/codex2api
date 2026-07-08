<p align="center">
  <img src="assets/banner.svg" alt="Codex2API" width="100%">
</p>

<p align="center">
  <a href="README.md">English</a> | <b>中文</b>
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

**把 Codex 账号池变成可观测、可调度、可运维的 OpenAI / Anthropic 兼容网关。** Codex2API 不是一个薄转发层，而是一套面向长期运行的 Codex 接入中枢：对外提供 `/v1/chat/completions`、`/v1/responses`、`/v1/messages`、Images 和 Models 等接口，对内维护 Refresh Token / Access Token 账号池、健康度评分、动态并发、限流恢复、用量统计和后台运维。

它可以跑在完整的 **PostgreSQL + Redis** 生产形态，也可以用 **SQLite + 内存缓存** 单容器轻量部署。你可以把它接到 Codex CLI、Claude Code、OpenAI SDK 或任何兼容客户端上，用一个统一 Base URL 管理多账号、代理池、API Key、Prompt 检查、生图工作台和运行时配置。

<table>
<tr><td width="210"><b>统一兼容入口</b></td><td>同时覆盖 OpenAI 风格 Chat Completions / Responses / Images、Anthropic Messages、无前缀兼容路由和 Codex 原生 Responses 转发，客户端侧少改配置即可接入。</td></tr>
<tr><td><b>账号池调度核心</b></td><td>围绕账号状态、健康层级、调度分、动态并发、冷却恢复和近期用量做选择，自动避开不可用账号，减少单账号打满和反复失败。支持 <code>round_robin</code> 和 <code>remaining_quota</code> 两种调度模式，以及单账号信用计费标记。</td></tr>
<tr><td><b>可视化管理后台</b></td><td>内置 React / Vite 管理台，提供账号导入测试、API Key、代理池、生图（文生图 + 图生图）、Prompt 检查、用量统计、运维概览、调度看板和系统设置。</td></tr>
<tr><td><b>两种部署形态</b></td><td>生产环境用 PostgreSQL + Redis，单机测试用 SQLite + Memory；Docker 镜像、源码构建、本地开发和一键交互部署脚本都已准备好。SQLite 模式默认绑定 <code>127.0.0.1</code> 以提升安全性。</td></tr>
<tr><td><b>计费与可观测性</b></td><td>单账号 5h/7d 窗口化 USD 费用追踪、信用配额支持、API Key 用量追踪、OAuth PKCE 获取 Token、Prompt 过滤，以及含请求日志与趋势图表的用量仪表盘。</td></tr>
</table>

---

## 在线 Demo

- Demo 地址：[https://codex2api-latest-vu8j.onrender.com](https://codex2api-latest-vu8j.onrender.com)
- Demo 密码：`codex2api`

> Demo 环境仅用于体验管理后台界面和基础功能，请勿上传真实 Refresh Token、Access Token、API Key 或其他敏感信息。

---

## 界面预览

> 以下截图使用演示数据，实际页面会随账号池、请求日志和运行环境变化。

![CodexProxy 仪表盘](docs/screenshots/dashboard.png)

<details>
<summary>查看更多后台界面</summary>

| 账号管理 | 仪表盘趋势 |
| --- | --- |
| ![账号管理](docs/screenshots/accounts.png) | ![仪表盘趋势](docs/screenshots/dashboard-trends.png) |

| 生图工作台 | Prompt 检查 |
| --- | --- |
| ![生图工作台](docs/screenshots/image-studio.png) | ![Prompt 检查](docs/screenshots/prompt-filter.png) |

| 系统运维 | 使用统计 |
| --- | --- |
| ![系统运维](docs/screenshots/operations.png) | ![使用统计](docs/screenshots/usage.png) |

| 使用文档 | API 文档 |
| --- | --- |
| ![使用文档](docs/screenshots/guide.png) | ![API 文档](docs/screenshots/api-reference.png) |

</details>

---

## 赞助商

> 想出现在这里？请到 GitHub 提交 Issue 联系。

<table>
<tr>
<td width="180" align="center" valign="middle"><a href="https://www.fastaitoken.com/register"><img src="assets/fastaitoken-logo.jpg" width="90" alt="FastAIToken"></a></td>
<td valign="middle"><b><a href="https://www.fastaitoken.com/register">FastAIToken</a></b> 是面向开发者的 AI API 聚合平台，支持 OpenAI、Claude、Gemini 等主流大模型，兼容 OpenAI API 协议，可无缝接入 Claude Code、Codex、Gemini CLI、Cherry Studio、Cline、Continue 等开发工具。采用充值 1:1（1 元 = 1 美元 API 额度），分组覆盖 0.02× OpenAI 福利（限时）到 1.2× Claude Max，并提供公开状态页与 7×24 小时真人技术支持。支持企业开票，提供 99% SLA 企业级稳定号池。</td>
</tr>
</table>

<table>
<tr>
<td width="180" align="center" valign="middle"><a href="https://aihub.top/register?aff=42WZVXN9KS4S"><img src="assets/aihub-logo.jpg" width="160" alt="AIHub"></a></td>
<td valign="middle"><b><a href="https://aihub.top/register?aff=42WZVXN9KS4S">AIHub</a></b> 是一家面向个人开发者和企业团队的高可用 AI 模型 API 中转平台。支持 Codex/Claude Code，价格大约是官方的 1 折不到！通过链接注册，使用优惠码 <code>CODEX2API</code> 即可获得 3$ 测试额度。</td>
</tr>
</table>

<table>
<tr>
<td width="180" align="center" valign="middle"><a href="https://ai.centos.hk"><b>星辰·AI</b></a></td>
<td valign="middle"><b><a href="https://ai.centos.hk">星辰·AI</a></b> 提供稳定、高速的 Claude Code / Codex / Gemini 中转服务，面向个人开发者与团队均适用。</td>
</tr>
</table>

---

## 目录

- [在线 Demo](#在线-demo)
- [界面预览](#界面预览)
- [赞助商](#赞助商)
- [快速部署](#快速部署)
  - [一键交互部署 (推荐)](#一键交互部署-推荐)
- [完整文档](#完整文档)
- [升级与本地开发](#升级与本地开发)
- [环境配置](#环境配置)
- [对外接口](#对外接口)
  - [Token 上传与账号管理](#token-上传与账号管理)
- [管理后台](#管理后台)
- [核心能力](#核心能力)
- [目录结构](#目录结构)
- [常见注意事项](#常见注意事项)
- [免责声明](#免责声明与开源协议)
- [Star History](#star-history)
- [友情链接](#友情链接)

---

## 快速部署

> 详细部署指南请参考：[DEPLOYMENT.md](docs/DEPLOYMENT.md)

### 一键交互部署 (推荐)

`deploy.sh` 提供 6 步交互式向导，依次询问 **端口 / 监听范围 / 数据库 / 密钥 / 构建方式 / 确认**，会先尝试拉取最新代码，再自动生成 `.env` 并拉起容器。

如果当前目录已有 `.env`，脚本会读取其中的端口、监听地址、数据库、Redis、管理密钥和 API 密钥作为默认值，后续重跑脚本时可直接回车复用原配置。

脚本会先检查当前系统是首次部署还是已有部署；如果发现已有 `.env` 或 compose 服务，会读取现有配置作为默认值，再进入完整部署向导。

**场景 1：尚未克隆仓库（一行远程拉起）**

```bash
bash <(curl -sSL https://raw.githubusercontent.com/james-6-23/codex2api/main/deploy.sh)
```

脚本会自动检测当前目录是否是 `codex2api` 仓库，若不是则克隆到 `./codex2api`，进入目录后再执行部署。

**场景 2：已经 `git clone` 到本地**

```bash
git clone https://github.com/james-6-23/codex2api.git
cd codex2api
bash deploy.sh
```

**监听范围选项**

| 选项 | 绑定地址 | 适用场景 |
| --- | --- | --- |
| 1) 仅本机访问 | `127.0.0.1` | 服务放在 nginx / Caddy 等反向代理后端，外网无法直接访问端口 |
| 2) 全部网络 (默认) | `0.0.0.0` | 直接通过服务器 IP 对外暴露，部署完成后会展示本机 / 内网 / 公网地址 |

绑定地址会写入 `.env` 的 `BIND_HOST`，后续可手动修改后 `docker compose up -d` 重启生效。

**可选环境变量**（用于自定义自举行为）

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `CODEX2API_REPO_URL` | `https://github.com/james-6-23/codex2api.git` | 克隆使用的仓库地址 |
| `CODEX2API_REPO_BRANCH` | `main` | 克隆使用的分支 |
| `CODEX2API_DIR_NAME` | `codex2api` | 克隆到本地的目录名 |
| `CODEX2API_SKIP_GIT_PULL` | 空 | 设为 `1` 或 `true` 时跳过部署前自动拉取最新代码 |

### 部署模式总览

| 模式 | 文件 | 适用场景 |
| --- | --- | --- |
| Docker 镜像部署 | `docker-compose.yml` | **推荐**，服务器 / 测试环境，直接拉取预构建镜像 |
| 本地源码容器构建 | `docker-compose.local.yml` | 本地改代码后做完整容器验证 |
| SQLite 轻量部署 | `docker-compose.sqlite.yml` | 单机轻量部署，不依赖 PostgreSQL / Redis |
| SQLite 本地源码构建 | `docker-compose.sqlite.local.yml` | 本地改代码后验证 SQLite 轻量模式 |
| 本地开发 | `go run .` + `npm run dev` | 前后端联调与调试 |

### 部署命令速查

标准镜像版：

```bash
git clone https://github.com/james-6-23/codex2api.git
cd codex2api
cp .env.example .env
docker compose pull
docker compose up -d
docker compose logs -f codex2api
```

标准本地构建版：

```bash
cp .env.example .env
docker compose -f docker-compose.local.yml up -d --build
docker compose -f docker-compose.local.yml logs -f codex2api
```

SQLite 镜像版：

```bash
cp .env.sqlite.example .env
docker compose -f docker-compose.sqlite.yml pull
docker compose -f docker-compose.sqlite.yml up -d
docker compose -f docker-compose.sqlite.yml logs -f codex2api
```

SQLite 本地构建版：

```bash
cp .env.sqlite.example .env
docker compose -f docker-compose.sqlite.local.yml up -d --build
docker compose -f docker-compose.sqlite.local.yml logs -f codex2api
```

补充说明：

- 标准版和 SQLite 版都读取 `.env`
- 切换部署模式前，需要先用对应的示例文件覆盖当前 `.env`
- 标准镜像版项目名固定为 `codex2api`，数据卷固定为 `codex2api_pgdata`、`codex2api_redisdata`
- 标准本地构建版项目名固定为 `codex2api-local`，数据卷固定为 `codex2api-local_pgdata`、`codex2api-local_redisdata`
- SQLite 镜像版项目名固定为 `codex2api-sqlite`，数据卷固定为 `codex2api-sqlite_sqlite-data`
- SQLite 本地构建版项目名固定为 `codex2api-sqlite-local`，数据卷固定为 `codex2api-sqlite-local_sqlite-data-local`
- 标准版容器名：`codex2api`
- SQLite 镜像版容器名：`codex2api-sqlite`
- SQLite 本地构建版容器名：`codex2api-sqlite-local`
- SQLite 轻量版只启动 `codex2api` 单容器，数据保存在 `/data/codex2api.db`
- **SQLite compose 文件默认绑定 `127.0.0.1`，仅本机可访问。** 如需暴露给外部，请在 `.env` 中设置 `BIND_HOST=0.0.0.0` 或修改 compose 文件中的端口绑定。标准版 compose 文件默认绑定 `0.0.0.0`（所有网络接口）。
- 生图工作台图库默认保存在 `/data/images`，上传的后台背景默认保存在 `/data/backgrounds`，标准版和 SQLite 版 Docker 配置都会持久化 `/data`
- `docker compose down` 默认不会删除命名卷；只有 `docker compose down -v`、`docker volume rm` 或 `docker volume prune` 才会删除持久化数据
- 不同部署模式的数据卷彼此隔离；切换 compose 文件后看到空数据，通常是切到了另一组卷，而不是原卷被自动删除

启动后访问：

- 管理台：`http://localhost:8080/admin/`
- 健康检查：`http://localhost:8080/health`

> 更多部署详情请参考：[DEPLOYMENT.md](docs/DEPLOYMENT.md)

---

## 完整文档

| 文档 | 说明 | 路径 |
|------|------|------|
| [API 文档](docs/API.md) | 所有 API 端点、请求/响应示例、错误码说明 | `docs/API.md` |
| [部署文档](docs/DEPLOYMENT.md) | 各种部署模式、升级指南、备份恢复 | `docs/DEPLOYMENT.md` |
| [配置文档](docs/CONFIGURATION.md) | 环境变量、系统设置、配置优先级 | `docs/CONFIGURATION.md` |
| [架构文档](docs/ARCHITECTURE.md) | 系统架构、调度算法、存储设计 | `docs/ARCHITECTURE.md` |
| [故障排查](docs/TROUBLESHOOTING.md) | 常见问题排查、诊断脚本、解决方案 | `docs/TROUBLESHOOTING.md` |
| [贡献指南](docs/CONTRIBUTING.md) | 开发规范、PR 流程、代码标准 | `docs/CONTRIBUTING.md` |
| [English README](README.md) | 英文项目介绍、部署和后台功能概览 | `README.md` |

---

## 升级与本地开发

```bash
git pull && docker compose pull && docker compose up -d && docker compose logs -f codex2api
```

> **⚠️ 重要：升级前请先备份数据库！**
>
> ```bash
> docker exec codex2api-postgres pg_dump -U codex2api codex2api > backup_$(date +%Y%m%d_%H%M%S).sql
> ```
>
> 如果升级后数据异常，可通过以下命令恢复：
>
> ```bash
> docker exec -i codex2api-postgres psql -U codex2api codex2api < backup_xxx.sql
> ```

如非必要，不建议在升级时执行 `docker compose down`；标准升级直接 `pull + up -d` 即可复用现有容器和命名卷。

### 本地开发模式

**后端：**

```bash
cp .env.example .env
cd frontend && npm ci && npm run build && cd ..
go run .
```

> 首次启动需要先构建前端，因为 Go 使用 `go:embed` 嵌入 `frontend/dist` 。

**前端开发服务器（联调）：**

```bash
cd frontend && npm ci && npm run dev
```

Vite 会自动代理 `/api` 和 `/health` 到后端，开发时访问 `http://localhost:5173/admin/`。

---

## 环境配置

### `.env` 环境变量

> 完整配置说明请参考：[CONFIGURATION.md](docs/CONFIGURATION.md)

| 变量 | 说明 |
| --- | --- |
| `CODEX_PORT` | HTTP 端口，默认 `8080` |
| `CODEX_MAX_REQUEST_BODY_SIZE_MB` | HTTP 请求体上限，单位 MB，默认 `48` |
| `ADMIN_SECRET` | 管理后台登录密钥；设置后首次访问 `/admin` 会弹出密码输入框 |
| `DATABASE_DRIVER` | 数据库驱动，支持 `postgres` / `sqlite` |
| `DATABASE_PATH` | SQLite 数据文件路径，`DATABASE_DRIVER=sqlite` 时生效 |
| `DATABASE_HOST` | PostgreSQL 主机，`DATABASE_DRIVER=postgres` 时生效 |
| `DATABASE_PORT` | PostgreSQL 端口，默认 `5432` |
| `DATABASE_USER` | PostgreSQL 用户 |
| `DATABASE_PASSWORD` | PostgreSQL 密码 |
| `DATABASE_NAME` | PostgreSQL 数据库名 |
| `DATABASE_SSLMODE` | PostgreSQL SSL 模式，默认 `disable` |
| `CACHE_DRIVER` | 缓存驱动，支持 `redis` / `memory` |
| `REDIS_ADDR` | Redis 地址，例如 `redis:6379`、`redis://default:pass@host:6379/0`、`rediss://default:pass@host:6379/0`，`CACHE_DRIVER=redis` 时生效 |
| `REDIS_USERNAME` | Redis ACL 用户名，可选；URL 中带用户名时可不填 |
| `REDIS_PASSWORD` | Redis 密码 |
| `REDIS_DB` | Redis DB 库号 |
| `REDIS_TLS` | 是否为 `host:port` 形式的 Redis 启用 TLS；使用 `rediss://` 时会自动启用 |
| `REDIS_INSECURE_SKIP_VERIFY` | 跳过 Redis TLS 证书校验，默认 `false`，仅用于自签证书或排障 |
| `TZ` | 时区，例如 `Asia/Shanghai` |

> Aiven、Upstash 等云 Redis 通常要求 TLS。推荐直接将 `REDIS_ADDR` 配置为平台提供的 `rediss://...` URL；如果只填写 `host:port`，请同时设置 `REDIS_TLS=true`。

标准版 `.env.example` 已显式声明 `DATABASE_DRIVER=postgres` 与 `CACHE_DRIVER=redis`；SQLite 轻量版请改用 `.env.sqlite.example`。

### 业务运行配置

以下参数**保存在数据库 `SystemSettings` 中**，通过管理台设置页面修改：

`MaxConcurrency`、`GlobalRPM`、`TestModel`、`TestContent`、`TestConcurrency`、`ProxyURL`、`PgMaxConns`、`RedisPoolSize`、`AdminSecret`、`SchedulerMode`、自动清理开关等。

首次启动时程序会自动写入默认设置。

### API Key 与管理密钥

- **对外 API Key**：以数据库中的 API Keys 为准。如果没有配置任何 Key，则 `/v1/*` 跳过鉴权。
- **管理后台 Admin Secret**：
  - 如果 `.env` 中设置了 `ADMIN_SECRET`，则优先使用环境变量。
  - 如果未设置 `ADMIN_SECRET`，则回退到数据库中的 `AdminSecret`。
  - 鉴权生效时，首次访问 `/admin` 会弹出密码输入框；前端登录成功后通过 `X-Admin-Key` 请求头访问 `/api/admin/*`。

---

## 对外接口

| 接口 | 说明 |
| --- | --- |
| `POST /v1/chat/completions` | Chat Completions 风格入口 |
| `POST /v1/responses` | Responses 风格入口 |
| `POST /v1/images/generations` | OpenAI Images 生成入口 |
| `POST /v1/images/edits` | OpenAI Images 编辑入口 |
| `GET /v1/models` | 返回可用模型列表（含 gpt-5.5、gpt-5.4、gpt-5.4-mini、gpt-5.3-codex、gpt-image-2 等） |
| `GET /health` | 健康检查 |

> **计费提示**：gpt-5.5 标准 tier 计费为 $5.00/M 输入 / $30.00/M 输出，priority tier 为 $12.50/M 输入 / $75.00/M 输出。其他模型按 billing 引擎规则计费。

> 完整请求/响应格式、错误码参见 [API 文档](docs/API.md)。

### Token 上传与账号管理

以下接口需要 `X-Admin-Key` 认证头。

#### 添加 Refresh Token 账号

```bash
# 单个添加
curl -X POST http://localhost:8080/api/admin/accounts \
  -H "X-Admin-Key: your-admin-secret" \
  -H "Content-Type: application/json" \
  -d '{"name": "my-account", "refresh_token": "rt_xxxxxxxxxxxx"}'

# 批量添加（换行分隔，单次最多 100 个）
curl -X POST http://localhost:8080/api/admin/accounts \
  -H "X-Admin-Key: your-admin-secret" \
  -H "Content-Type: application/json" \
  -d '{"name": "batch", "refresh_token": "rt_xxx1\nrt_xxx2\nrt_xxx3"}'
```

#### 添加 Access Token 账号（AT-only）

```bash
# 单个添加
curl -X POST http://localhost:8080/api/admin/accounts/at \
  -H "X-Admin-Key: your-admin-secret" \
  -H "Content-Type: application/json" \
  -d '{"name": "my-at", "access_token": "eyJhbGciOiJSUzI1NiIs..."}'

# 批量添加（换行分隔）
curl -X POST http://localhost:8080/api/admin/accounts/at \
  -H "X-Admin-Key: your-admin-secret" \
  -H "Content-Type: application/json" \
  -d '{"access_token": "eyJtoken1...\neyJtoken2...\neyJtoken3..."}'
```

#### 文件批量导入

```bash
# 导入 Refresh Token（TXT，每行一个）
curl -X POST http://localhost:8080/api/admin/accounts/import \
  -H "X-Admin-Key: your-admin-secret" \
  -F "file=@tokens.txt" \
  -F "format=txt"

# 导入 Refresh Token（JSON 格式）
curl -X POST http://localhost:8080/api/admin/accounts/import \
  -H "X-Admin-Key: your-admin-secret" \
  -F "file=@credentials.json" \
  -F "format=json"

# 导入 Access Token（AT-TXT，每行一个）
curl -X POST http://localhost:8080/api/admin/accounts/import \
  -H "X-Admin-Key: your-admin-secret" \
  -F "file=@access_tokens.txt" \
  -F "format=at_txt"
```

> 所有导入接口自动去重，已存在的 Token 不会重复写入。更多管理接口（导出、迁移、OAuth 授权等）参见 [API 文档](docs/API.md)。

#### OAuth PKCE 授权

Codex2API 支持通过 OAuth PKCE 流程获取 Refresh Token，适用于无法手动提取 Token 的场景：

```bash
# 步骤 1：生成授权 URL
curl -X POST http://localhost:8080/api/admin/oauth/generate-auth-url \
  -H "X-Admin-Key: your-admin-secret" \
  -H "Content-Type: application/json" \
  -d '{}'

# 步骤 2：在浏览器中打开返回的 auth_url，完成授权
# 步骤 3：用授权码兑换 Token（自动创建账号）
curl -X POST http://localhost:8080/api/admin/oauth/exchange-code \
  -H "X-Admin-Key: your-admin-secret" \
  -H "Content-Type: application/json" \
  -d '{"session_id": "...", "code": "...", "state": "..."}'
```

> 完整 OAuth 流程及所有管理接口参见 [API 文档](docs/API.md)。

---

## 管理后台

浏览器访问 `/admin/`，提供以下页面：

| 页面 | 路径 | 功能 |
| --- | --- | --- |
| Dashboard | `/admin/` | 总览指标、请求趋势、延迟趋势、Token 分布、模型排行 |
| 账号管理 | `/admin/accounts` | 导入、测试、批量处理、调度信息查看 |
| API 密钥 | `/admin/api-keys` | API Key 创建、查看、删除与调用凭据管理 |
| 代理管理 | `/admin/proxies` | 代理池维护、账号代理分配与连通性管理 |
| 生图工作台 | `/admin/images/studio` | 文生图、图生图、提示词模板、任务历史和服务器图库 |
| Prompt 检查 | `/admin/prompt-filter/overview` | Prompt 规则、触发日志、测试和处理模式配置 |
| 使用统计 | `/admin/usage` | 请求日志、统计卡片、图表、日志清空 |
| 运维概览 | `/admin/ops` | 运行态监控与系统概览 |
| 调度看板 | `/admin/ops/scheduler` | 调度健康度、惩罚项和评分拆解 |
| 系统设置 | `/admin/settings` | 业务运行参数与后台密钥配置 |
| 使用文档 | `/admin/docs` | Codex CLI / Claude Code 接入示例 |
| API 文档 | `/admin/api-reference` | OpenAI 风格接口与管理接口参考 |

---

## 核心能力

### 项目定位

这个项目不是单纯的接口转发，而是一套面向长期运行的 Codex 网关与管理后台：

- 对外提供统一的 OpenAI 风格入口，屏蔽上游多账号差异
- 对内维护基于 `Refresh Token` 的账号池、`Access Token` 生命周期和运行时调度
- 通过 PostgreSQL + Redis 或 SQLite + 内存缓存实现配置持久化与运行态协调
- 通过 `/admin` 管理台提供全面的运维观测能力

### 架构概览

**对外请求链路：** 客户端请求 → Gin RPM 限流 → `proxy.Handler` API Key 校验 → `auth.Store` 调度选号 → 上游请求 → 响应回传 + 用量写入

**管理后台链路：** 浏览器 → `/admin/` 嵌入式前端 → `/api/admin/*` 管理接口 → 数据库 / 账号池 / 缓存层

### 调度系统

调度核心位于 `auth.Store`，将账号可用性、健康度、动态并发、历史错误和近期用量综合纳入选择。

**运行时状态模型：**

- `Status`：`ready` / `cooldown` / `error`
- `HealthTier`：`healthy` / `warm` / `risky` / `banned`
- `SchedulerScore`：以 100 为基线的实时调度分
- `DynamicConcurrencyLimit`：按健康层级动态收缩的并发上限

**账号选择策略：**

1. 过滤不可用账号（error / banned / 冷却中 / 无 AccessToken）
2. 重算健康层级、调度分和动态并发
3. 排除已达并发上限的账号
4. 按 `healthy > warm > risky > banned` 排序，同层级按调度分和并发数择优
5. 15% 概率随机打散，降低热点与饥饿

**动态并发规则：**

| 层级 | 并发上限 |
| --- | --- |
| `healthy` | 系统 `MaxConcurrency` |
| `warm` | 基础并发 ÷ 2（最少 1） |
| `risky` | 固定 1 |
| `banned` | 固定 0，不参与调度 |

**调度分惩罚/奖励：**

| 信号 | 影响 |
| --- | --- |
| `unauthorized` | `-50`，24h 线性衰减 |
| `rate_limited` | `-22`，1h 线性衰减 |
| `timeout` | `-18`，15min 线性衰减 |
| `server error` | `-12`，15min 线性衰减 |
| 连续失败 | 每次 `-6`，最多 `-24` |
| 连续成功 | 每次 `+2`，最多 `+12` |
| 近期成功率过低 | `<75%` 扣 8，`<50%` 扣 15 |
| Free 7d 用量 | `≥70%` 扣 8 → `≥100%` 扣 40 |
| 延迟 EWMA | `≥5s` 扣 4 → `≥20s` 扣 15 |

**冷却与恢复机制：**

- **429**：优先解析上游 `resets_at`，否则按套餐类型推断冷却时间
- **401**：直接进入 `banned`，6h 冷却，24h 内再触发升至 24h
- 冷却状态持久化到 PostgreSQL，重启后自动恢复
- 后台会对 `banned` 账号做周期性低频恢复探测

**调度可观测性：**

- `GET /api/admin/accounts` — 健康层级、调度分、惩罚拆解
- `GET /api/admin/ops/overview` — 系统运行态与连接池概览
- `/admin/ops/scheduler` — 前端调度看板

**调度模式**（`scheduler_mode`，通过管理后台设置）：

| 模式 | 行为 |
| --- | --- |
| `round_robin`（默认） | 按健康层级轮询可用账号，权重按调度分排序 |
| `remaining_quota` | 优先使用用量较低的账号；用量相同时轮询 |

**信用账号**（单账号标记）：

对采用信用计费而非 Free/Pro 用量计费的账号，可标记为信用账号以跳过用量窗口惩罚：

| 字段 | 类型 | 作用 |
| --- | --- | --- |
| `credit_enabled` | bool | 标记账号为信用计费模式 |
| `credit_skip_usage_window` | bool | 开启后跳过 7 天/5 小时用量窗口惩罚 |

**窗口化 USD 费用**：账号列表展示每个账号在两个时间窗口内的累计计费金额——过去 5 小时和过去 7 天，窗口对齐各账号的用量重置边界。这反映的是实际扣费金额而非估算的 Token 费用。

---

## 目录结构

```text
codex2api/
├─ main.go                      # 程序入口
├─ Dockerfile                   # 多阶段镜像构建
├─ docker-compose.yml           # 镜像部署模板
├─ docker-compose.local.yml     # 本地源码构建模板
├─ .env.example                 # 环境变量示例
├─ admin/                       # 管理后台 API
├─ auth/                        # 账号池、调度与 token 管理
├─ cache/                       # Redis 缓存封装
├─ config/                      # 环境变量加载
├─ database/                    # 数据库访问层
├─ proxy/                       # 对外代理、转发与限流
└─ frontend/                    # React + Vite 管理后台
   ├─ src/pages/                # Dashboard / Accounts / API Keys / Proxies / Images / Prompt Filter / Ops / Usage / Settings / Docs
   ├─ src/components/           # UI 组件
   ├─ src/locales/              # 国际化语言文件 (zh/en)
   └─ vite.config.js            # Vite 配置
```

---


## 常见注意事项

- `docker-compose.yml` 拉取 GHCR 镜像用于部署；`docker-compose.local.yml` 用 `build: .` 做本地构建
- 前端基路径固定为 `/admin/`，本地开发和生产部署一致
- 本地手动构建 Go 二进制前需先执行 `frontend/` 的 `npm run build`
- `.env` 只负责端口、数据库、Redis 等物理层配置；业务参数在管理台数据库里维护
- API Key 以数据库为准，在管理台中配置

---

## 免责声明与开源协议

- 本项目仅供学习、研究与技术交流使用。
- 本项目采用 `MIT License` 开源协议。
- 项目不对任何直接或间接使用后果提供担保；生产环境使用风险由使用者自行承担。

---

## Star History

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="https://api.star-history.com/svg?repos=james-6-23/codex2api&type=Date&theme=dark" />
  <source media="(prefers-color-scheme: light)" srcset="https://api.star-history.com/svg?repos=james-6-23/codex2api&type=Date" />
  <img alt="Star History Chart" src="https://api.star-history.com/svg?repos=james-6-23/codex2api&type=Date" />
</picture>

---

## 友情链接

- [LINUX DO](https://linux.do/)
