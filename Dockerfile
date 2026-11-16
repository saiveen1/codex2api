# syntax=docker/dockerfile:1

# ============================================================
# Stage 1: 构建前端 (React + Vite)
# 前端产物是纯静态文件，只需构建一次，与目标平台无关
# ============================================================
FROM --platform=$BUILDPLATFORM node:20-alpine AS frontend-builder

ARG BUILD_VERSION=dev

WORKDIR /frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm \
    npm ci --no-audit --no-fund
COPY frontend/ .
RUN VITE_APP_VERSION=${BUILD_VERSION} npm run build

# ============================================================
# Stage 2: 构建 Go 后端
# 使用 BUILDPLATFORM 原生运行 + TARGETARCH 交叉编译
# ============================================================
FROM --platform=$BUILDPLATFORM golang:1.26.11-alpine AS go-builder

ARG TARGETARCH
ARG BUILD_VERSION=dev

# 国内构建走 goproxy.cn，避免直连 proxy.golang.org 断流（unexpected EOF）
ENV GOPROXY=https://goproxy.cn,direct

WORKDIR /app
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
COPY --from=frontend-builder /frontend/dist ./frontend/dist

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -ldflags="-s -w -X github.com/codex2api/internal/version.Version=${BUILD_VERSION}" -o /codex2api .

# Bundle the existing locked worker environment; enabling it remains opt-in.
FROM ghcr.io/astral-sh/uv:0.11.25 AS uv
FROM python:3.13.7-slim-bookworm AS web-worker-builder
COPY --from=uv /uv /usr/local/bin/uv
WORKDIR /opt/chatgpt-web-worker
COPY services/chatgpt-web-worker/pyproject.toml services/chatgpt-web-worker/uv.lock ./
RUN --mount=type=cache,target=/root/.cache/uv \
    UV_LINK_MODE=copy uv sync --locked --no-dev --no-managed-python

# ============================================================
# Stage 3: 最终运行镜像（内置可选 Web Worker）
# ============================================================
FROM python:3.13.7-slim-bookworm

RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates tzdata wget \
    && rm -rf /var/lib/apt/lists/*
COPY --from=web-worker-builder /opt/chatgpt-web-worker/.venv /opt/chatgpt-web-worker/.venv
COPY services/chatgpt-web-worker/*.py services/chatgpt-web-worker/LICENSE services/chatgpt-web-worker/NOTICE services/chatgpt-web-worker/PROVENANCE.md services/chatgpt-web-worker/README.md /opt/chatgpt-web-worker/
COPY services/chatgpt-web-worker/pyproject.toml services/chatgpt-web-worker/uv.lock /opt/chatgpt-web-worker/
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1

COPY --from=go-builder /codex2api /usr/local/bin/codex2api

# 默认 LOG_DIR=logs 是相对路径；固定工作目录才能落到 compose 挂载的 /app/logs，
# 否则审计日志会写到容器根目录的 /logs，容器重建后丢失。
WORKDIR /app

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/codex2api"]
