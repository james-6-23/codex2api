# syntax=docker/dockerfile:1

# ============================================================
# Stage 1: 构建前端 (React + Vite)
# 前端产物是纯静态文件，只需构建一次，与目标平台无关
# ============================================================
FROM --platform=$BUILDPLATFORM node:20-alpine AS frontend-builder

ARG BUILD_VERSION=dev

WORKDIR /frontend
COPY frontend/package.json frontend/package-lock.json ./
# npm 在弱网下会以 "Exit handler never called!" 退出却返回 0，留下一个空的
# node_modules，直到下一步 vite not found 才暴露。多给几次重试，并显式校验产物。
RUN --mount=type=cache,target=/root/.npm \
    npm ci --no-audit --no-fund --fetch-retries 5 --fetch-retry-maxtimeout 120000 \
    && test -x node_modules/.bin/vite
COPY frontend/ .
RUN VITE_APP_VERSION=${BUILD_VERSION} npm run build

# ============================================================
# Stage 2: 构建 Go 后端
# 使用 BUILDPLATFORM 原生运行 + TARGETARCH 交叉编译
# ============================================================
FROM --platform=$BUILDPLATFORM golang:1.26.5-alpine AS go-builder

ARG TARGETARCH
ARG BUILD_VERSION=dev

# 国内构建走 goproxy.cn，避免直连 proxy.golang.org 断流（unexpected EOF）。
# 出海机器上 goproxy.cn 常常直接超时，用 --build-arg GOPROXY=https://proxy.golang.org,direct 覆盖。
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}

WORKDIR /app
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
COPY --from=frontend-builder /frontend/dist ./frontend/dist

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -ldflags="-s -w -X github.com/codex2api/internal/version.Version=${BUILD_VERSION}" -o /codex2api .

# ============================================================
# Stage 3: 最终运行镜像
# ============================================================
FROM alpine:3.19

# 不走 apk：Alpine CDN 在部分机房被拦，而这两样东西不必联网取。
# 时区库已由 main.go 的 _ "time/tzdata" 编进二进制，只剩根证书要带。
COPY --from=go-builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

COPY --from=go-builder /codex2api /usr/local/bin/codex2api

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/codex2api"]
