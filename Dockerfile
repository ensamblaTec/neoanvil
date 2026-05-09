# NeoAnvil — multi-stage build (Area 1.1.A)
#
# Layout:
#   Stage 1 (deps)        : pre-warms the Go module cache from go.work + the
#                           15 go.mod manifests. Layer is invalidated only
#                           when a manifest changes, so subsequent code
#                           edits skip the slow `go mod download`.
#   Stage 2 (hud-builder) : Vite/TS build → cmd/neo-nexus/static/. Output
#                           is consumed by the next stage via go:embed.
#   Stage 3 (go-builder)  : compiles every binary as a static, CGO-free
#                           ELF. Pure-Go; Zero-CGO is a project invariant.
#   Stage 4 (runtime)     : alpine + ca-certificates, copies the binaries
#                           and a writable /data volume mount point for
#                           Nexus + child workspaces.
#
# CPU baseline: GOAMD64=v3 (Haswell+/Zen1+, 2013/2017). Override with
# `--build-arg GOAMD64=v1` for lowest-common-denominator portability.

ARG GO_VERSION=1.26
ARG NODE_VERSION=22
ARG ALPINE_VERSION=3.20
ARG GOAMD64=v3

# ─────────────────────────── Stage 1: deps ────────────────────────────
FROM golang:${GO_VERSION}-alpine AS deps
WORKDIR /app

# Copy go.work + every go.mod listed there. The deps layer is cached
# until any of these manifests changes — code edits don't bust it.
COPY go.work go.work.sum* ./
COPY go.mod go.sum ./
COPY cmd/neo/go.mod         cmd/neo/go.sum*         ./cmd/neo/
COPY cmd/neo-mcp/go.mod     cmd/neo-mcp/go.sum*     ./cmd/neo-mcp/
COPY cmd/neo-tui/go.mod     cmd/neo-tui/go.sum*     ./cmd/neo-tui/
COPY pkg/astx/go.mod          pkg/astx/go.sum*          ./pkg/astx/
COPY pkg/config/go.mod        pkg/config/go.sum*        ./pkg/config/
COPY pkg/mctx/go.mod          pkg/mctx/go.sum*          ./pkg/mctx/
COPY pkg/memx/go.mod          pkg/memx/go.sum*          ./pkg/memx/
COPY pkg/mesh/go.mod          pkg/mesh/go.sum*          ./pkg/mesh/
COPY pkg/observability/go.mod pkg/observability/go.sum* ./pkg/observability/
COPY pkg/rag/go.mod           pkg/rag/go.sum*           ./pkg/rag/
COPY pkg/swarm/go.mod         pkg/swarm/go.sum*         ./pkg/swarm/
COPY pkg/telemetry/go.mod     pkg/telemetry/go.sum*     ./pkg/telemetry/
COPY pkg/tensorx/go.mod       pkg/tensorx/go.sum*       ./pkg/tensorx/
COPY pkg/wasmx/go.mod         pkg/wasmx/go.sum*         ./pkg/wasmx/

ENV GOWORK=/app/go.work \
    GOFLAGS=-mod=readonly
RUN go mod download all

# ─────────────────────────── Stage 2: hud-builder ─────────────────────
FROM node:${NODE_VERSION}-alpine AS hud-builder
WORKDIR /app/web

COPY web/package.json web/package-lock.json* ./
# `npm install` (not `npm ci`) — package-lock.json is gitignored in this
# repo by design (matches `make build-hud` semantics). Reproducibility
# trade-off accepted: hud-builder pulls latest minor versions matching
# package.json semver. If reproducibility is later required, drop the
# package-lock from .gitignore + switch to `npm ci`.
RUN npm install --no-audit --no-fund

COPY web/ ./
RUN npm run build

# Mirror Makefile build-hud staging exactly so go:embed sees the same
# layout in either Docker or `make build-hud` flows.
RUN mkdir -p /staged/static/assets \
 && cp dist/index.html /staged/static/index.html \
 && cp -r dist/assets/* /staged/static/assets/ \
 && [ -f dist/favicon.svg ] && cp dist/favicon.svg /staged/static/ || true \
 && [ -f dist/icons.svg ]   && cp dist/icons.svg   /staged/static/ || true

# ─────────────────────────── Stage 3: go-builder ──────────────────────
FROM golang:${GO_VERSION}-alpine AS go-builder
ARG GOAMD64
WORKDIR /app

# Reuse the populated module cache from stage 1.
COPY --from=deps /go/pkg/mod /go/pkg/mod
COPY --from=deps /root/.cache/go-build /root/.cache/go-build

# Bring in the source tree (the repo is intentionally lean; .dockerignore
# strips bin/, .neo/db/, web/node_modules/, etc.).
COPY . .

# Stage the freshly built SPA on top of any committed bundle so go:embed
# in cmd/neo-nexus/dashboard.go picks up the runtime-built version.
COPY --from=hud-builder /staged/static/ ./cmd/neo-nexus/static/

ENV CGO_ENABLED=0 \
    GOOS=linux \
    GOARCH=amd64 \
    GOAMD64=${GOAMD64} \
    GOWORK=/app/go.work

# One pure-Go binary per cmd/. Plugin-echo is the test fixture and is
# excluded just like the Makefile's build-plugins target does.
RUN mkdir -p /out \
 && go build -trimpath -ldflags="-s -w" -o /out/neo-mcp           ./cmd/neo-mcp \
 && go build -trimpath -ldflags="-s -w" -o /out/neo-nexus         ./cmd/neo-nexus \
 && go build -trimpath -ldflags="-s -w" -o /out/neo               ./cmd/neo \
 && go build -trimpath -ldflags="-s -w" -o /out/neo-plugin-jira     ./cmd/plugin-jira \
 && go build -trimpath -ldflags="-s -w" -o /out/neo-plugin-deepseek ./cmd/plugin-deepseek

# ─────────────────────────── Stage 4: runtime ─────────────────────────
FROM alpine:${ALPINE_VERSION} AS runtime

# `su-exec` is a ~10KB static helper (alpine-native gosu equivalent)
# used by docker-entrypoint.sh to drop from root → neo without forking.
# `wget` is BusyBox built-in (used by the compose healthcheck).
RUN apk add --no-cache ca-certificates tzdata su-exec \
 && addgroup -S neo \
 && adduser  -S -G neo -h /home/neo neo \
 && mkdir -p /home/neo/.neo /home/neo/.neo-seed /home/neo/work \
 && chown -R neo:neo /home/neo

COPY --from=go-builder /out/neo-mcp             /usr/local/bin/neo-mcp
COPY --from=go-builder /out/neo-nexus           /usr/local/bin/neo-nexus
COPY --from=go-builder /out/neo                 /usr/local/bin/neo
COPY --from=go-builder /out/neo-plugin-jira     /usr/local/bin/neo-plugin-jira
COPY --from=go-builder /out/neo-plugin-deepseek /usr/local/bin/neo-plugin-deepseek

# Seed template — entrypoint copies this to ~/.neo/nexus.yaml on first
# boot if the volume doesn't already have one. Lives under .neo-seed/
# (NOT .neo/) so the named volume mount at .neo/ doesn't shadow it.
COPY nexus.yaml.example /home/neo/.neo-seed/nexus.yaml

# Entrypoint runs as root, reconciles volume ownership + seeds nexus.yaml,
# then drops to `neo` via su-exec. Do NOT set `USER neo` here — the
# entrypoint MUST start as root to chown the named volumes.
COPY scripts/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

WORKDIR /home/neo

# Nexus dispatcher (9000) + dashboard HUD (8087). Child workspaces bind
# to dynamic ports inside 9100-9299 and are not reachable from outside
# the container (Nexus proxies into them).
EXPOSE 9000 8087

# Single entrypoint: Nexus owns the process tree (children inherit fd's,
# so they MUST live in the same container).
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
