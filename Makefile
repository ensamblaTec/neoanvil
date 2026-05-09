# NeoAnvil build + restart targets.
# [SRE-101.D] rebuild-restart automates the binary-stale trap: recompile both
# neo-mcp and neo-nexus, then SIGHUP the dispatcher (or restart if no PID).
# [PILAR-XXIII/171] Auto-detects host OS/ARCH and selects the optimal SIMD
# level (GOAMD64=v3 for Intel Haswell+/Zen1+, GOARM64=v8.2 for Apple Silicon
# M1-M4, Graviton3+) — portable, pure Go, no CGO, no assembly files.

# Use bash for recipes so builtins like `disown` work (default /bin/sh → dash).
SHELL := /bin/bash

.PHONY: all help archinfo cpufeat bench bench-cache bench-compare bench-affinity \
        build build-mcp build-nexus build-cli build-tui build-migrate-quant build-plugins audit-plugins install-git-hooks \
        build-generic build-fast build-all build-hud \
        restart rebuild-restart kill-stale test clean freshness yaml-audit audit audit-baseline audit-ci \
        capture-profile build-pgo rebuild-restart-pgo pgo-auto \
        bench-baseline audit-bench

GO          ?= go
BIN_DIR     := bin
MCP_BIN     := $(BIN_DIR)/neo-mcp
NEXUS_BIN   := $(BIN_DIR)/neo-nexus
CLI_BIN     := $(BIN_DIR)/neo

# Make Go-installed binaries (staticcheck, ineffassign, modernize, deadcode) visible
# to recipe shells when the operator's shell PATH omits $(go env GOPATH)/bin.
export PATH := $(shell $(GO) env GOPATH)/bin:$(PATH)

# ═════════════════════════════════════════════════════════════════════
# Architecture detection + SIMD feature auto-selection [PILAR-XXIII/171]
# ═════════════════════════════════════════════════════════════════════
# The compiler flag GOAMD64 (amd64) or GOARM64 (arm64) tells Go to emit
# SIMD instructions (AVX2/FMA/BMI2 or NEON+fma+crypto) in auto-vectorized
# loops. 100% portable — no .s assembly files, no CGO — but gives 3-5×
# speedup on vector dot product, BM25 scoring, HNSW distance, and any
# other hot loop over []float32 or []byte.
#
# Levels:
#   GOAMD64=v1 — baseline x86-64 (2003+)              — no SIMD auto-vec
#   GOAMD64=v2 — Intel Nehalem+ / AMD Bulldozer+      — SSE3/SSE4
#   GOAMD64=v3 — Intel Haswell+ / AMD Zen1+ (default) — AVX2/FMA/BMI2
#   GOAMD64=v4 — Intel Skylake-X+ / AMD Zen4+         — AVX-512
#   GOARM64=v8.2 — Apple M1+, Graviton3+ (default)    — NEON+fma+crypto
#   GOARM64=v9.0 — Apple M2+ (some), Graviton4+       — SVE2

DETECTED_OS    := $(shell uname -s | tr '[:upper:]' '[:lower:]')
DETECTED_ARCH  := $(shell uname -m)

# Normalize arch name → Go's GOARCH convention.
ifeq ($(DETECTED_ARCH),x86_64)
    GOARCH_DETECTED := amd64
else ifeq ($(DETECTED_ARCH),aarch64)
    GOARCH_DETECTED := arm64
else ifeq ($(DETECTED_ARCH),arm64)
    GOARCH_DETECTED := arm64
else
    GOARCH_DETECTED := $(DETECTED_ARCH)
endif

# Select default SIMD level. Override from shell: `GOAMD64=v4 make build`.
ifeq ($(GOARCH_DETECTED),amd64)
    GOAMD64_LEVEL ?= v3
    SIMD_ENV      := GOAMD64=$(GOAMD64_LEVEL)
    SIMD_LABEL    := GOAMD64=$(GOAMD64_LEVEL)
else ifeq ($(GOARCH_DETECTED),arm64)
    GOARM64_LEVEL ?= v8.2
    SIMD_ENV      := GOARM64=$(GOARM64_LEVEL)
    SIMD_LABEL    := GOARM64=$(GOARM64_LEVEL)
else
    SIMD_ENV      :=
    SIMD_LABEL    := portable-baseline
endif

GOBUILD := $(SIMD_ENV) GOOS=$(DETECTED_OS) GOARCH=$(GOARCH_DETECTED) $(GO) build -trimpath

# Convenience preambles for target logs.
define banner_arch
	@printf "\033[36m[make]\033[0m building for \033[1m%s/%s\033[0m with \033[33m%s\033[0m\n" \
		"$(DETECTED_OS)" "$(GOARCH_DETECTED)" "$(SIMD_LABEL)"
endef

# ═════════════════════════════════════════════════════════════════════
# Default targets
# ═════════════════════════════════════════════════════════════════════
all: build

help:
	@printf "%b\n" "NeoAnvil Makefile — targets:"
	@printf "%b\n" ""
	@printf "%b\n" "  \033[1mBuild (auto-detected host):\033[0m"
	@printf "%b\n" "    build              build all 3 binaries (mcp + nexus + cli)"
	@printf "%b\n" "    build-mcp          build only neo-mcp"
	@printf "%b\n" "    build-nexus        build only neo-nexus"
	@printf "%b\n" "    build-cli          build only the neo CLI"
	@printf "%b\n" "    build-tui          build the Bubbletea TUI dashboard"
	@printf "%b\n" "    build-hud          compile React SPA + stage into cmd/neo-nexus/static/"
	@printf "%b\n" "    build-migrate-quant  build the offline int8/binary migration report CLI"
	@printf "%b\n" ""
	@printf "%b\n" "  \033[1mBuild variants:\033[0m"
	@printf "%b\n" "    build-generic      portable baseline (GOAMD64=v1) — runs on any x86-64"
	@printf "%b\n" "    build-fast         maximum SIMD (GOAMD64=v4 / GOARM64=v9.0)"
	@printf "%b\n" "    build-all          cross-compile for linux+darwin × amd64+arm64"
	@printf "%b\n" ""
	@printf "%b\n" "  \033[1mRun / restart:\033[0m"
	@printf "%b\n" "    restart            SIGHUP the running neo-nexus (hot-reload)"
	@printf "%b\n" "    rebuild-restart    full cycle: rebuild + kill stale + relaunch nexus"
	@printf "%b\n" ""
	@printf "%b\n" "  \033[1mDiagnostics:\033[0m"
	@printf "%b\n" "    archinfo           print detected OS/ARCH + selected SIMD flag"
	@printf "%b\n" "    cpufeat            print CPU feature flags (avx2, avx512, neon...)"
	@printf "%b\n" "    freshness          compare binary mtime vs last cmd/pkg commit"
	@printf "%b\n" "    yaml-audit         detect schema drift neo.yaml ↔ config.go"
	@printf "%b\n" "    audit              run staticcheck+ineffassign+modernize+cover"
	@printf "%b\n" "    audit-baseline     capture current audit output as the CI baseline"
	@printf "%b\n" "    audit-ci           fail-on-new: diff against .neo/audit-baseline.txt"
	@printf "%b\n" ""
	@printf "%b\n" "  \033[1mTest / bench:\033[0m"
	@printf "%b\n" "    test               go test -short ./pkg/..."
	@printf "%b\n" "    bench              run pkg/rag benchmarks with current SIMD flag"
	@printf "%b\n" "    bench-cache        focused benchmarks for PILAR XXV cache stack"
	@printf "%b\n" "    bench-compare      run pkg/rag benchmarks at v1 vs v3 — quantifies speedup"
	@printf "%b\n" ""
	@printf "%b\n" "  \033[1mPGO (Profile-Guided Optimization, Épica 364):\033[0m"
	@printf "%b\n" "    capture-profile    scrape 60s pprof from running neo-mcp → default.pgo"
	@printf "%b\n" "    build-pgo          rebuild with -pgo=auto (requires default.pgo)"
	@printf "%b\n" "    rebuild-restart-pgo  capture → build-pgo → restart Nexus (full cycle)"
	@printf "%b\n" ""
	@printf "%b\n" "  \033[1mBenchmark regression gate (Épica 366.B):\033[0m"
	@printf "%b\n" "    bench-baseline     run bench + write .neo/bench-baseline.json (run post-fix)"
	@printf "%b\n" "    audit-bench        bench vs baseline, fail on >5% ns/op or any new allocs"
	@printf "%b\n" ""
	@printf "%b\n" "  \033[1mClean:\033[0m"
	@printf "%b\n" "    clean              rm -rf bin/"
	@printf "%b\n" ""
	@printf "%b\n" "  \033[1mOverrides (shell env):\033[0m"
	@printf "%b\n" "    GOAMD64=v4 make build    force AVX-512 build (requires Skylake-X+/Zen4+)"
	@printf "%b\n" "    GOAMD64=v1 make build    fallback to generic (any x86-64)"
	@printf "%b\n" "    GOARM64=v9.0 make build  force SVE2 on arm64"

# ═════════════════════════════════════════════════════════════════════
# Diagnostics
# ═════════════════════════════════════════════════════════════════════

# archinfo: show detected machine capabilities + active build flags.
archinfo:
	@printf "\033[1m─────────────────────────────────────────────────────────\033[0m\n"
	@printf "  OS:          %s\n" "$(DETECTED_OS)"
	@printf "  ARCH:        %s → GOARCH=%s\n" "$(DETECTED_ARCH)" "$(GOARCH_DETECTED)"
	@printf "  SIMD flag:   \033[33m%s\033[0m\n" "$(SIMD_LABEL)"
	@printf "  Go:          %s\n" "$$($(GO) version | cut -d' ' -f3-)"
	@printf "\033[1m─────────────────────────────────────────────────────────\033[0m\n"

# cpufeat: print the instruction set extensions reported by the host.
# Lets the operator confirm the selected GOAMD64/GOARM64 level matches
# the actual CPU. Portable between Linux and macOS.
cpufeat:
ifeq ($(DETECTED_OS),linux)
	@printf "\033[1mCPU features (from /proc/cpuinfo):\033[0m\n"
	@grep -m1 "model name" /proc/cpuinfo | sed 's/^/  /'
	@printf "\n  SIMD flags present: "
	@grep -m1 "flags" /proc/cpuinfo | tr ' ' '\n' | grep -iE \
		"^(sse[0-9_]*|avx[0-9_]*|fma|bmi[0-9]|aes|sha_ni|vnni|vaes|f16c)$$" | \
		sort -u | tr '\n' ' '
	@printf "\n\n  \033[32mRecommended:\033[0m "
	@flags=$$(grep -m1 "flags" /proc/cpuinfo); \
		if echo "$$flags" | grep -q "avx512f"; then echo "GOAMD64=v4 (AVX-512 available)"; \
		elif echo "$$flags" | grep -q "avx2"; then echo "GOAMD64=v3 (AVX2 available — default is optimal)"; \
		elif echo "$$flags" | grep -q "sse4_2"; then echo "GOAMD64=v2 (SSE4 only — old CPU)"; \
		else echo "GOAMD64=v1 (no SIMD — very old CPU)"; fi
else ifeq ($(DETECTED_OS),darwin)
	@printf "\033[1mCPU features (macOS sysctl):\033[0m\n"
	@sysctl -n machdep.cpu.brand_string 2>/dev/null | sed 's/^/  /' || true
	@printf "\n"
	@if [ "$(GOARCH_DETECTED)" = "arm64" ]; then \
		printf "  Apple Silicon detected → \033[32mGOARM64=v8.2 (NEON+fma+crypto)\033[0m is optimal.\n"; \
		printf "  M2+ with SME may benefit from GOARM64=v9.0 (opt-in, not all M2 variants support it).\n"; \
	else \
		printf "  Intel Mac — "; \
		sysctl -n machdep.cpu.features machdep.cpu.leaf7_features 2>/dev/null | \
			tr ' ' '\n' | grep -iE "^(SSE[0-9_]*|AVX[0-9_]*|FMA|BMI[0-9])$$" | \
			sort -u | tr '\n' ' '; \
		printf "\n"; \
	fi
else
	@printf "%b\n" "cpufeat not supported on $(DETECTED_OS) — use shell env to override GOAMD64."
endif

# ═════════════════════════════════════════════════════════════════════
# Build targets — host-optimized
# ═════════════════════════════════════════════════════════════════════

build: build-mcp build-nexus build-cli

build-mcp:
	$(call banner_arch)
	$(GOBUILD) -o $(MCP_BIN) ./cmd/neo-mcp

# build-nexus: by default does NOT rebuild the HUD SPA (web/) — it relies on
# the bundle already staged under cmd/neo-nexus/static/. Run `make build-hud`
# manually when the React SPA changes (requires npm + tsc installed locally).
build-nexus:
	$(call banner_arch)
	$(GOBUILD) -o $(NEXUS_BIN) ./cmd/neo-nexus

# install-git-hooks: PILAR XXIII / Épica 127.E — install scripts/git-hooks/*
# into .git/hooks/ via symlink (so updates land automatically).
# Backs up any existing hook to <name>.bak. Idempotent: existing
# symlinks pointing to scripts/git-hooks/ are silently kept.
#
# Currently installed:
#   post-commit — auto-document Jira tickets referenced in commit msg
#                 via prepare_doc_pack action (zero-token automation)
#   commit-msg  — soft-warn when [EPIC-FINAL MCPI-N] points at a
#                 non-existent ticket (134.B.3)
#
# Helpers ending in .sh (e.g. sync-master-plan.sh, called by post-commit)
# are skipped — git only invokes hooks named exactly after the lifecycle
# events (pre-commit, commit-msg, post-commit, ...).
install-git-hooks:
	$(call banner_arch)
	@if [ ! -d .git ]; then \
		printf "\033[31m[install-git-hooks]\033[0m not a git repo, aborting\n"; \
		exit 1; \
	fi
	@for hook in scripts/git-hooks/*; do \
		name=$$(basename $$hook); \
		case "$$name" in *.sh) continue ;; esac; \
		[ -f "$$hook" ] || continue; \
		dst=".git/hooks/$$name"; \
		if [ -L "$$dst" ] && [ "$$(readlink "$$dst")" = "../../scripts/git-hooks/$$name" ]; then \
			printf "\033[36m[install-git-hooks]\033[0m %s already linked\n" "$$name"; \
			continue; \
		fi; \
		if [ -f "$$dst" ]; then \
			mv "$$dst" "$$dst.bak"; \
			printf "\033[33m[install-git-hooks]\033[0m %s backed up to %s.bak\n" "$$name" "$$name"; \
		fi; \
		ln -s "../../scripts/git-hooks/$$name" "$$dst"; \
		chmod +x "$$hook"; \
		printf "\033[32m[install-git-hooks]\033[0m %s installed\n" "$$name"; \
	done

# audit-plugins: PILAR XXIII / Épica 126.3 — runs go vet + staticcheck +
# gosec on each cmd/plugin-* in isolation. Each plugin is its own
# attack surface (talks to external APIs with secrets); auditing them
# separately keeps findings scoped and surfaces vendor-specific issues
# (Atlassian REST patterns, OAuth flows) without diluting in a repo-wide
# scan. Skips plugin-echo (test fixture) by default; PLUGINS_INCLUDE_ECHO=1
# to include. Fails on first plugin with findings.
audit-plugins:
	$(call banner_arch)
	@plugins=$$(ls -d cmd/plugin-* 2>/dev/null); \
	 if [ -z "$$plugins" ]; then \
		printf "\033[33m[audit-plugins]\033[0m no cmd/plugin-* found; nothing to audit\n"; \
		exit 0; \
	 fi; \
	 for p in $$plugins; do \
		name=$$(basename $$p); \
		if [ "$$name" = "plugin-echo" ] && [ -z "$$PLUGINS_INCLUDE_ECHO" ]; then \
		  printf "\033[36m[audit-plugins]\033[0m skipping %s (test fixture)\n" "$$name"; \
		  continue; \
		fi; \
		printf "\033[36m[audit-plugins]\033[0m %s\n" "$$name"; \
		printf "  \033[2mgo vet\033[0m\n"; \
		$(GO) vet ./$$p/... || exit 1; \
		if command -v staticcheck >/dev/null 2>&1; then \
		  printf "  \033[2mstaticcheck\033[0m\n"; \
		  staticcheck ./$$p/... || exit 1; \
		else \
		  printf "  \033[33m[skip]\033[0m staticcheck not installed (go install honnef.co/go/tools/cmd/staticcheck@latest)\n"; \
		fi; \
		if command -v gosec >/dev/null 2>&1; then \
		  printf "  \033[2mgosec\033[0m\n"; \
		  gosec -quiet ./$$p/... || exit 1; \
		else \
		  printf "  \033[33m[skip]\033[0m gosec not installed (go install github.com/securego/gosec/v2/cmd/gosec@latest)\n"; \
		fi; \
	 done
	@printf "\033[32m[audit-plugins]\033[0m all plugins clean\n"

# build-plugins: PILAR XXIII / Épica 126.2 — compiles every cmd/plugin-* into
# bin/. Add new plugins to the cmd/plugin-* glob and they get picked up
# automatically. Skips cmd/plugin-echo (the test fixture) by default —
# pass PLUGINS_INCLUDE_ECHO=1 to include it.
build-plugins:
	$(call banner_arch)
	@plugins=$$(ls -d cmd/plugin-* 2>/dev/null | sed 's|cmd/||'); \
	 if [ -z "$$plugins" ]; then \
		printf "\033[33m[build-plugins]\033[0m no cmd/plugin-* found; nothing to do\n"; \
		exit 0; \
	 fi; \
	 for p in $$plugins; do \
		if [ "$$p" = "plugin-echo" ] && [ -z "$$PLUGINS_INCLUDE_ECHO" ]; then \
		  printf "\033[36m[build-plugins]\033[0m skipping %s (test fixture; PLUGINS_INCLUDE_ECHO=1 to include)\n" "$$p"; \
		  continue; \
		fi; \
		out="$(BIN_DIR)/neo-$$p"; \
		printf "\033[36m[build-plugins]\033[0m %-25s -> %s\n" "$$p" "$$out"; \
		$(GOBUILD) -o "$$out" ./cmd/$$p || exit 1; \
	 done

# ═════════════════════════════════════════════════════════════════════
# PGO (Profile-Guided Optimization) — PILAR LXIX / Épica 364.A
# ═════════════════════════════════════════════════════════════════════
# Typical gain: 2–7% on CPU-bound hot paths (HNSW search, SIMD kernels,
# embedder round-trip). Flow:
#   1. `make build` + `make rebuild-restart` (current binary running)
#   2. Generate representative workload via scripts/pgo-workload.sh OR just
#      let agents use neoanvil for ~60s of real tool calls.
#   3. `make capture-profile` — scrapes /debug/pprof/profile from the SRE
#      diagnostics server (default :9371) and writes default.pgo.
#   4. `make build-pgo` — recompiles with -pgo=auto. The compiler inlines
#      hot functions (cosineAVX2, Graph.Search, etc.) and tunes branch
#      prediction to real-world probabilities.
#   5. Optional: `make rebuild-restart-pgo` cycles to the PGO binary.
#
# default.pgo is a binary blob — do NOT commit (gitignored). Refresh
# periodically; workload drift stales the profile.

# capture-profile: pull a 60-second CPU profile from the running neo-mcp
# diagnostics server and save it as default.pgo. Duration overridable via
# PROFILE_SECONDS env var.
PROFILE_SECONDS ?= 60
PROFILE_PORT    ?= 9371
capture-profile:
	@printf "\033[36m[PGO]\033[0m capturing %ss profile from http://127.0.0.1:$(PROFILE_PORT)/debug/pprof/profile\n" "$(PROFILE_SECONDS)"
	@curl -s -o default.pgo "http://127.0.0.1:$(PROFILE_PORT)/debug/pprof/profile?seconds=$(PROFILE_SECONDS)" || \
		(printf "\033[31m[PGO]\033[0m capture failed — is neo-mcp running on :%s ?\n" "$(PROFILE_PORT)" && exit 1)
	@size=$$(wc -c < default.pgo | tr -d ' '); \
	 if [ "$$size" -lt 100 ]; then \
	   printf "\033[31m[PGO]\033[0m captured file suspiciously small (%s bytes) — neo-mcp may be idle\n" "$$size"; \
	   exit 1; \
	 fi; \
	 printf "\033[32m[PGO]\033[0m captured \033[1mdefault.pgo\033[0m (%s bytes)\n" "$$size"
	@$(GO) tool pprof -top -cum default.pgo 2>/dev/null | head -10 | sed 's/^/    /' || true

# build-pgo: build all binaries with -pgo=auto. Detects default.pgo in repo
# root automatically. Fails cleanly if the profile is missing — forcing a
# capture first prevents accidentally shipping un-optimized "PGO" builds.
build-pgo: default.pgo
	@printf "\033[1m\033[36m[make]\033[0m building with PGO (default.pgo present)\n"
	$(GOBUILD) -pgo=auto -o $(MCP_BIN) ./cmd/neo-mcp
	$(GOBUILD) -pgo=auto -o $(NEXUS_BIN) ./cmd/neo-nexus
	$(GOBUILD) -pgo=auto -o $(CLI_BIN) ./cmd/neo
	@printf "\033[32m[make]\033[0m PGO build complete. Verify embedded profile:\n"
	@$(GO) tool buildid $(MCP_BIN) 2>/dev/null | sed 's/^/    /' || true

default.pgo:
	@printf "\033[31m[PGO]\033[0m default.pgo not found — run \033[1mmake capture-profile\033[0m first\n"
	@printf "    (requires a running neo-mcp with workload — idle capture produces empty profiles)\n"

# pgo-auto: pick newest .neo/pgo/profile-<unix>.pgo (written by ContinuousPGOCapture
# when sre.pgo_capture_interval_minutes > 0) and copy to default.pgo. Chain:
#   make pgo-auto build-pgo    # auto-captured
#   make capture-profile build-pgo  # on-demand 60s grab
# [PILAR LXIX / 364.C]
pgo-auto:
	@latest=$$(ls -t .neo/pgo/profile-*.pgo 2>/dev/null | head -1); \
	 if [ -z "$$latest" ]; then \
	   printf "\033[31m[PGO]\033[0m no captured profiles in .neo/pgo/ — enable sre.pgo_capture_interval_minutes in neo.yaml or run make capture-profile\n"; \
	   exit 1; \
	 fi; \
	 cp "$$latest" default.pgo && \
	 printf "\033[32m[PGO]\033[0m auto-picked \033[1m%s\033[0m → default.pgo\n" "$$latest"
	@exit 1

# rebuild-restart-pgo: convenience wrapper — captures a fresh profile from
# the currently running binary, rebuilds with PGO, and cycles Nexus. Use
# this ONCE the binary has been running under realistic workload for ≥60s.
rebuild-restart-pgo: capture-profile build-pgo
	@printf "\033[36m[make]\033[0m cycling with PGO-optimized binary\n"
	@$(MAKE) rebuild-restart

# ═════════════════════════════════════════════════════════════════════
# Benchmark regression gate — PILAR LXIX / Épica 366.B
# ═════════════════════════════════════════════════════════════════════
# Prevents PRs from silently regressing the hot-path performance that
# 364/365/366.A/367/368 invest in. Baseline lives in .neo/bench-baseline.json
# (commit it); audit-bench runs benchmarks and fails on >5% ns/op regression
# or any non-zero allocation growth.
#
# Workflow after landing a perf-improving épica:
#   make bench-baseline      # capture new numbers
#   git add .neo/bench-baseline.json && git commit
#
# CI gate on every PR:
#   make audit-bench

# BENCH_PATTERN selects which benchmarks to include in baseline/audit. Keep
# tight — only arch-agnostic, sub-second benches that reflect hot paths.
# Override with BENCH_PATTERN='BenchmarkFoo|BenchmarkBar' env when iterating.
BENCH_PATTERN ?= Benchmark(LexicalIndex|QueryCache|QuantizeBinary|HammingDistance|HammingSimilarity)
BENCH_DIRS    ?= ./pkg/rag/...

bench-baseline:
	@printf "\033[36m[bench]\033[0m capturing baseline (pattern: $(BENCH_PATTERN))\n"
	@mkdir -p .neo
	@$(GO) test -bench='$(BENCH_PATTERN)' -run='^$$' -benchtime=100ms -benchmem $(BENCH_DIRS) > /tmp/neo-bench-baseline.txt 2>/tmp/neo-bench-baseline.err
	@python3 scripts/bench-regression.py --capture /tmp/neo-bench-baseline.txt > .neo/bench-baseline.json 2>/dev/null
	@count=$$(grep -c '"ns_op"' .neo/bench-baseline.json 2>/dev/null || echo 0); \
	 printf "\033[32m[bench]\033[0m baseline: %s benchmarks captured in .neo/bench-baseline.json\n" "$$count"

audit-bench: .neo/bench-baseline.json
	@printf "\033[36m[bench]\033[0m running vs baseline (commit $$(python3 -c 'import json; print(json.load(open(".neo/bench-baseline.json"))["commit"][:8])'))\n"
	@$(GO) test -bench='$(BENCH_PATTERN)' -run='^$$' -benchtime=100ms -benchmem $(BENCH_DIRS) > /tmp/neo-bench-current.txt 2>&1
	@python3 scripts/bench-regression.py .neo/bench-baseline.json /tmp/neo-bench-current.txt

.neo/bench-baseline.json:
	@printf "\033[31m[bench]\033[0m .neo/bench-baseline.json missing — run \033[1mmake bench-baseline\033[0m first\n"
	@exit 1

# build-hud: compile the React SPA and stage it under cmd/neo-nexus/static/
# so the go:embed in dashboard.go picks up the freshest bundle.
# Decoupled from build-nexus — the staged bundle is committed-equivalent and
# only needs to be rebuilt when web/src/* changes. Requires npm + tsc.
# [PILAR-XXVII/245.Q]
build-hud:
	@command -v npm >/dev/null 2>&1 || { printf "\033[31m[hud]\033[0m npm not found — skip HUD rebuild (existing bundle in cmd/neo-nexus/static/ is used)\n"; exit 1; }
	@[ -d web/node_modules ] || { printf "\033[33m[hud]\033[0m web/node_modules missing — running 'npm install' in web/\n"; cd web && npm install; }
	@printf "\033[36m[make]\033[0m building HUD SPA (web/ → cmd/neo-nexus/static/)\n"
	@cd web && npm run build 2>&1 | tail -4
	@mkdir -p cmd/neo-nexus/static/assets
	@rm -f cmd/neo-nexus/static/assets/*.js cmd/neo-nexus/static/assets/*.css
	@cp web/dist/index.html cmd/neo-nexus/static/index.html
	@cp -r web/dist/assets/* cmd/neo-nexus/static/assets/
	@[ -f web/dist/favicon.svg ] && cp web/dist/favicon.svg cmd/neo-nexus/static/ || true
	@[ -f web/dist/icons.svg ]   && cp web/dist/icons.svg   cmd/neo-nexus/static/ || true
	@printf "\033[36m[make]\033[0m HUD staged → $$(ls cmd/neo-nexus/static/assets/ | wc -l) asset(s)\n"

build-cli:
	$(call banner_arch)
	$(GOBUILD) -o $(CLI_BIN) ./cmd/neo

# build-tui: Bubbletea TUI dashboard. Separate go.mod (workspace member)
# so its heavy lipgloss/bubbles deps don't pollute the main module graph.
# [Épica 238]
build-tui:
	$(call banner_arch)
	@cd cmd/neo-tui && $(SIMD_ENV) GOOS=$(DETECTED_OS) GOARCH=$(GOARCH_DETECTED) \
		$(GO) build -trimpath -o ../../$(BIN_DIR)/neo-tui .

# build-migrate-quant: offline CLI that builds int8/binary companion
# arrays on an existing HNSW WAL and reports RAM overhead + search
# latency. Requires neo-mcp stopped (bbolt lock is exclusive). [Épica 170.D]
build-migrate-quant:
	$(call banner_arch)
	$(GOBUILD) -o $(BIN_DIR)/neo-migrate-quant ./cmd/neo-migrate-quant

# build-generic: compatibility build that runs on ANY x86-64 (including pre-2013
# CPUs without AVX2). Use when shipping to heterogeneous fleet or CI runners
# without known feature set. On arm64, defaults to minimal GOARM64=v8.0.
build-generic:
	@printf "\033[36m[make]\033[0m building \033[1mgeneric\033[0m (no SIMD auto-vec)\n"
	@GOAMD64=v1 GOARM64=v8.0 GOOS=$(DETECTED_OS) GOARCH=$(GOARCH_DETECTED) \
		$(GO) build -trimpath -o $(MCP_BIN) ./cmd/neo-mcp
	@GOAMD64=v1 GOARM64=v8.0 GOOS=$(DETECTED_OS) GOARCH=$(GOARCH_DETECTED) \
		$(GO) build -trimpath -o $(NEXUS_BIN) ./cmd/neo-nexus
	@GOAMD64=v1 GOARM64=v8.0 GOOS=$(DETECTED_OS) GOARCH=$(GOARCH_DETECTED) \
		$(GO) build -trimpath -o $(CLI_BIN) ./cmd/neo

# build-fast: maximum SIMD (AVX-512 or SVE2). Requires matching CPU — will
# crash at boot if the host doesn't support the feature set.
build-fast:
ifeq ($(GOARCH_DETECTED),amd64)
	@printf "\033[36m[make]\033[0m building \033[1mfast\033[0m with \033[33mGOAMD64=v4 (AVX-512)\033[0m\n"
	@GOAMD64=v4 GOOS=$(DETECTED_OS) GOARCH=amd64 $(GO) build -trimpath -o $(MCP_BIN) ./cmd/neo-mcp
	@GOAMD64=v4 GOOS=$(DETECTED_OS) GOARCH=amd64 $(GO) build -trimpath -o $(NEXUS_BIN) ./cmd/neo-nexus
	@GOAMD64=v4 GOOS=$(DETECTED_OS) GOARCH=amd64 $(GO) build -trimpath -o $(CLI_BIN) ./cmd/neo
else ifeq ($(GOARCH_DETECTED),arm64)
	@printf "\033[36m[make]\033[0m building \033[1mfast\033[0m with \033[33mGOARM64=v9.0 (SVE2)\033[0m\n"
	@GOARM64=v9.0 GOOS=$(DETECTED_OS) GOARCH=arm64 $(GO) build -trimpath -o $(MCP_BIN) ./cmd/neo-mcp
	@GOARM64=v9.0 GOOS=$(DETECTED_OS) GOARCH=arm64 $(GO) build -trimpath -o $(NEXUS_BIN) ./cmd/neo-nexus
	@GOARM64=v9.0 GOOS=$(DETECTED_OS) GOARCH=arm64 $(GO) build -trimpath -o $(CLI_BIN) ./cmd/neo
else
	@printf "%b\n" "build-fast not supported on $(GOARCH_DETECTED)"
endif

# build-all: cross-compile for every supported (os,arch) pair, tagged with
# the SIMD level used. Outputs end in bin/ with suffix -<os>-<arch>-<level>.
# Ideal for CI release pipelines — Mac M3 Ultra, Intel Ultra 9, Linux ARM
# servers all served from the same CI run.
build-all:
	@printf "\033[36m[make]\033[0m cross-compiling full matrix\n"
	@for cmd in neo-mcp neo-nexus neo; do \
		echo "  linux/amd64/v3   → $(BIN_DIR)/$$cmd-linux-amd64-v3"; \
		GOAMD64=v3 GOOS=linux GOARCH=amd64 $(GO) build -trimpath \
			-o $(BIN_DIR)/$$cmd-linux-amd64-v3 ./cmd/$$cmd; \
		echo "  linux/arm64/v8.2 → $(BIN_DIR)/$$cmd-linux-arm64-v8.2"; \
		GOARM64=v8.2 GOOS=linux GOARCH=arm64 $(GO) build -trimpath \
			-o $(BIN_DIR)/$$cmd-linux-arm64-v8.2 ./cmd/$$cmd; \
		echo "  darwin/amd64/v3  → $(BIN_DIR)/$$cmd-darwin-amd64-v3"; \
		GOAMD64=v3 GOOS=darwin GOARCH=amd64 $(GO) build -trimpath \
			-o $(BIN_DIR)/$$cmd-darwin-amd64-v3 ./cmd/$$cmd; \
		echo "  darwin/arm64/v8.2 → $(BIN_DIR)/$$cmd-darwin-arm64-v8.2"; \
		GOARM64=v8.2 GOOS=darwin GOARCH=arm64 $(GO) build -trimpath \
			-o $(BIN_DIR)/$$cmd-darwin-arm64-v8.2 ./cmd/$$cmd; \
	done
	@printf "%b\n" "[make] cross-compile done."

# ═════════════════════════════════════════════════════════════════════
# Test / benchmark
# ═════════════════════════════════════════════════════════════════════

test:
	$(GO) test -short ./pkg/...

# test-integration: Area 3.3.B + 1.3.D — exercises the subprocess plugin
# tests under cmd/plugin-jira, cmd/plugin-deepseek, and cmd/neo-nexus.
# The tests self-compile their own binaries via `go build` inside t.TempDir(),
# so the explicit `make build-plugins` is a sanity gate that fails fast if
# any plugin won't link. Filter `Integration|E2E` is package-scoped to the
# three subprocess test suites; broader pkg-level E2E tests run under `make test`.
.PHONY: test-integration
test-integration: build-plugins
	@printf "\033[36m[make]\033[0m running plugin subprocess + Nexus E2E integration tests\n"
	$(GO) test -race -timeout 5m -count=1 \
		-run 'Integration|E2E' \
		./cmd/plugin-jira/... ./cmd/plugin-deepseek/... ./cmd/neo-nexus/...

# bench: RAG hot-path benchmarks with the active SIMD flag. Results printed
# to stdout; pipe to tee if you want to keep them.
bench:
	@printf "\033[36m[make]\033[0m benchmarking \033[33m%s\033[0m\n" "$(SIMD_LABEL)"
	@$(SIMD_ENV) $(GO) test ./pkg/rag/ -bench . -benchmem -benchtime=2s -run '^$$' 2>&1 | \
		tail -n +1

# bench-cache: focused benchmark harness for the PILAR XXV cache stack
# (174/179/199). Runs just the Cache/QueryCache/TextCache/Hamming/
# CosineDistance benchmarks so operators tuning capacity see their
# delta in <10 s instead of the full bench's 40+ s. [Épica 219]
bench-cache:
	@printf "\033[36m[make]\033[0m benchmarking cache stack (\033[33m%s\033[0m)\n" "$(SIMD_LABEL)"
	@$(SIMD_ENV) $(GO) test ./pkg/rag/ -bench 'QueryCache|TextCache|LexicalIndex|HammingDistance|CosineDistance|DotProduct|PopulateInt8|PopulateBinary' \
		-benchmem -benchtime=1s -run '^$$' 2>&1 | tail -n +1
	@printf "\n\033[36m[make]\033[0m observability tracker benchmarks\n"
	@$(SIMD_ENV) $(GO) test ./pkg/observability/ -bench . -benchmem -benchtime=1s -run '^$$' 2>&1 | tail -n +1

# bench-compare: runs the RAG benchmarks first at v1 (no SIMD), then at v3
# (AVX2), and surfaces a human-readable diff. Quantifies the actual speedup
# from auto-vectorization on your specific CPU. Handy for the README or
# architecture decisions. Linux/amd64 only (macOS users: run twice manually).
bench-compare:
ifeq ($(GOARCH_DETECTED),amd64)
	@printf "\033[36m[make]\033[0m benchmark comparison v1 vs v3\n"
	@mkdir -p /tmp/neo-bench
	@printf "%b\n" "───── baseline (GOAMD64=v1, no SIMD auto-vec) ─────"
	@GOAMD64=v1 $(GO) test ./pkg/rag/ -bench . -benchmem -benchtime=2s -run '^$$' \
		> /tmp/neo-bench/v1.txt 2>&1 && tail -n 20 /tmp/neo-bench/v1.txt
	@printf "%b\n" ""
	@printf "%b\n" "───── optimized (GOAMD64=v3, AVX2/FMA/BMI2) ───────"
	@GOAMD64=v3 $(GO) test ./pkg/rag/ -bench . -benchmem -benchtime=2s -run '^$$' \
		> /tmp/neo-bench/v3.txt 2>&1 && tail -n 20 /tmp/neo-bench/v3.txt
	@printf "%b\n" ""
	@printf "%b\n" "───── summary ─────"
	@printf "  baseline file: /tmp/neo-bench/v1.txt\n"
	@printf "  optimized:     /tmp/neo-bench/v3.txt\n"
	@printf "  to diff:       \033[33mbenchstat /tmp/neo-bench/v1.txt /tmp/neo-bench/v3.txt\033[0m\n"
	@printf "  (install benchstat: go install golang.org/x/perf/cmd/benchstat@latest)\n"
else
	@printf "%b\n" "bench-compare requires amd64 host (currently $(GOARCH_DETECTED))"
endif

# bench-affinity: compares HNSW search latency across three thread-pinning modes. [367.A]
# Runs NoAffinity, LockOSThreadOnly, and FullAffinity benchmarks and prints a diff.
# Gate: FullAffinity.p99 <= LockOSThreadOnly.p99 * 0.97 AND ops_sec >= LockOSThreadOnly * 1.02
# Install benchstat: go install golang.org/x/perf/cmd/benchstat@latest
bench-affinity:
	@printf "\033[36m[make]\033[0m CPU affinity benchmark — three modes\n"
	@mkdir -p /tmp/neo-bench
	@printf "%b\n" "───── NoAffinity (baseline) ─────"
	@$(GO) test ./pkg/rag/ -bench 'BenchmarkHNSWSearch_NoAffinity' -benchmem -benchtime=3s -count=3 -run '^$$' \
		| tee /tmp/neo-bench/affinity-none.txt
	@printf "%b\n" "───── LockOSThreadOnly ─────"
	@$(GO) test ./pkg/rag/ -bench 'BenchmarkHNSWSearch_LockOSThreadOnly' -benchmem -benchtime=3s -count=3 -run '^$$' \
		| tee /tmp/neo-bench/affinity-lock.txt
	@printf "%b\n" "───── FullAffinity (SchedSetaffinity) ─────"
	@$(GO) test ./pkg/rag/ -bench 'BenchmarkHNSWSearch_FullAffinity' -benchmem -benchtime=3s -count=3 -run '^$$' \
		| tee /tmp/neo-bench/affinity-full.txt
	@printf "%b\n" ""
	@printf "  to diff: \033[33mbenchstat /tmp/neo-bench/affinity-lock.txt /tmp/neo-bench/affinity-full.txt\033[0m\n"
	@printf "  gate: FullAffinity p99 <= LockOSThreadOnly p99 * 0.97 AND ops/sec >= 1.02×\n"

# ═════════════════════════════════════════════════════════════════════
# Run / restart
# ═════════════════════════════════════════════════════════════════════

# restart: sends SIGHUP to the running Nexus dispatcher (hot-reload config +
# reconcile pool). If no Nexus is running, this is a no-op.
restart:
	@pid=$$(pgrep -x neo-nexus 2>/dev/null); \
	if [ -n "$$pid" ]; then \
		echo "[make] SIGHUP neo-nexus (pid=$$pid)"; \
		kill -HUP $$pid; \
	else \
		echo "[make] no neo-nexus process found"; \
	fi

# kill-stale: pre-kill sweep executed BEFORE the build step so that BoltDB
# locks are released during compilation, not after. Also prevents double-Nexus
# scenario by killing ALL instances (pkill vs single-PID kill). [ÉPICA 271.A]
# Also reads .neo/neo-mcp.pid files from registered workspaces to kill by exact
# PID (complement to pkill for processes with non-standard argv[0]). [ÉPICA 271.C]
kill-stale:
	@printf "%b\n" "[make] zombie guard: terminating stale neo-nexus and neo-mcp"
	@pkill -TERM neo-nexus 2>/dev/null; sleep 1; pkill -KILL neo-nexus 2>/dev/null || true
	@pkill -TERM neo-mcp   2>/dev/null; sleep 1; pkill -KILL neo-mcp   2>/dev/null || true
	@python3 -c "import json,os,pathlib,signal; \
	  ws_file=pathlib.Path.home()/'.neo/workspaces.json'; \
	  [( (pf:=pathlib.Path(w['path'])/'.neo/neo-mcp.pid').exists() and \
	     (pid:=int(pf.read_text().strip())) and \
	     ([os.kill(pid,signal.SIGTERM),print(f'[make] pid-lock killed {pid} ({w[\"name\"]})')] \
	      if __import__(\"os\").path.exists(f'/proc/{pid}') else pf.unlink(missing_ok=True)) ) \
	   for w in (json.loads(ws_file.read_text()).get('workspaces',[]) if ws_file.exists() else [])]" \
	  2>/dev/null || true
	@printf "%b\n" "[make] stale processes cleared"

# rebuild-restart: full cycle — pre-kill stale processes, recompile both
# binaries, then relaunch Nexus. Children are respawned with the new neo-mcp.
# kill-stale runs FIRST (before build) so BoltDB locks are released during
# compilation. The post-build kill block below is defense-in-depth for any
# new processes that spawned during the build phase.
# Compatible with macOS and Linux (no ss/stat -c/tail --pid/xargs -r).
rebuild-restart: kill-stale build-mcp build-nexus build-cli build-plugins
	@pid=$$(pgrep -x neo-nexus 2>/dev/null); \
	if [ -n "$$pid" ]; then \
		echo "[make] stopping neo-nexus (pid=$$pid)"; \
		kill $$pid 2>/dev/null; \
		for i in 1 2 3 4 5 6 7 8 9 10; do \
			if ! kill -0 $$pid 2>/dev/null; then break; fi; \
			sleep 1; \
		done; \
		if kill -0 $$pid 2>/dev/null; then \
			echo "[make] graceful stop timeout — escalating to SIGKILL"; \
			kill -9 $$pid 2>/dev/null; \
			sleep 1; \
		fi; \
	fi
	@children=$$(pgrep -x neo-mcp 2>/dev/null); \
	if [ -n "$$children" ]; then \
		echo "[make] SIGTERM neo-mcp children: $$children (graceful — lets caches flush)"; \
		echo "$$children" | xargs kill -TERM 2>/dev/null; \
		for i in 1 2 3 4 5; do \
			alive=""; \
			for pid in $$children; do \
				if kill -0 $$pid 2>/dev/null; then alive="$$alive $$pid"; fi; \
			done; \
			[ -z "$$alive" ] && break; \
			sleep 1; \
		done; \
		alive=""; \
		for pid in $$children; do \
			if kill -0 $$pid 2>/dev/null; then alive="$$alive $$pid"; fi; \
		done; \
		if [ -n "$$alive" ]; then \
			echo "[make] SIGTERM timeout (5s) — escalating to SIGKILL:$$alive"; \
			for pid in $$alive; do kill -9 $$pid 2>/dev/null; done; \
			sleep 1; \
		fi; \
	fi
	@for i in 1 2 3 4 5 6 7 8 9 10; do \
		if ! lsof -ti :9000 > /dev/null 2>&1 && ! lsof -ti :8087 > /dev/null 2>&1; then break; fi; \
		echo "[make] waiting for ports 9000/8087 to release..."; \
		sleep 1; \
	done
	@printf "%b\n" "[make] launching fresh neo-nexus"
	@./$(NEXUS_BIN) > /tmp/neo-nexus.log 2>&1 & \
	NEXUS_PID=$$!; \
	echo "[make] neo-nexus pid=$$NEXUS_PID"; \
	sleep 4; \
	if curl -sf http://127.0.0.1:9000/health > /dev/null; then \
		echo "[make] dispatcher up — starting workspaces not yet running"; \
		first=$$(curl -s http://127.0.0.1:9000/status 2>/dev/null | python3 -c "import sys,json; ws=json.load(sys.stdin); print(ws[0]['id'] if ws else '')" 2>/dev/null); \
		stopped=$$(curl -s http://127.0.0.1:9000/status 2>/dev/null | python3 -c "import sys,json; [print(w['id']) for w in json.load(sys.stdin) if w.get('status') not in ('running','starting')]" 2>/dev/null); \
		for id in $$stopped; do \
			curl -s -X POST "http://127.0.0.1:9000/api/v1/workspaces/start/$$id" > /dev/null && echo "  started $$id (was stopped)"; \
		done; \
		echo "[make] verifying children reach running state (up to 30s)..."; \
		for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do \
			stopped=$$(curl -s http://127.0.0.1:9000/status 2>/dev/null | python3 -c "import sys,json; s=json.load(sys.stdin); print(sum(1 for w in s if w.get('status')!='running'))" 2>/dev/null); \
			if [ "$$stopped" = "0" ]; then echo "  ✓ all children running"; break; fi; \
			sleep 2; \
		done; \
		if [ -n "$$first" ]; then \
			curl -s -X PUT -d "{\"id\":\"$$first\"}" http://127.0.0.1:9000/api/v1/workspaces/active > /dev/null && echo "  active=$$first (OAuth proxy enabled)"; \
		fi; \
		curl -s http://127.0.0.1:9000/status | python3 -c "import sys,json;[print(f'  child {w[\"id\"]} port={w[\"port\"]} status={w[\"status\"]} pid={w[\"pid\"]}') for w in json.load(sys.stdin)]"; \
		echo "[make] ── tailing /tmp/neo-nexus.log — Ctrl+C stops neo-nexus ──"; \
		tail -n 30 -f /tmp/neo-nexus.log & \
		TAIL_PID=$$!; \
		trap "echo; echo '[make] stopping neo-nexus (pid='$$NEXUS_PID')'; kill $$NEXUS_PID 2>/dev/null; kill $$TAIL_PID 2>/dev/null; exit 0" INT; \
		wait $$NEXUS_PID 2>/dev/null; \
		kill $$TAIL_PID 2>/dev/null; \
	else \
		echo "[make] dispatcher not responding"; \
		tail -10 /tmp/neo-nexus.log; \
		kill $$NEXUS_PID 2>/dev/null; \
	fi

clean:
	rm -rf $(BIN_DIR)

# yaml-audit: detect schema drift between pkg/config NeoConfig yaml tags and
# the keys present in neo.yaml.example. Prevents new config fields from
# shipping without operator-facing documentation. [SRE-114.C]
yaml-audit:
	@$(GO) run -tags=script scripts/yaml-schema-check.go

# audit: one-shot tech-debt scan of the entire repo. [Épica 237]
# Runs staticcheck + ineffassign + modernize + go test -short -cover and
# groups the output into a compact table. Installs missing tools into
# GOBIN on first run.
audit:
	@printf "\033[1m─────────── NeoAnvil tech-debt audit ───────────\033[0m\n"
	@command -v staticcheck >/dev/null 2>&1 || $(GO) install honnef.co/go/tools/cmd/staticcheck@latest 2>/dev/null
	@command -v ineffassign >/dev/null 2>&1 || $(GO) install github.com/gordonklaus/ineffassign@latest 2>/dev/null
	@command -v modernize >/dev/null 2>&1 || $(GO) install golang.org/x/tools/gopls/internal/analysis/modernize/cmd/modernize@latest 2>/dev/null
	@command -v deadcode >/dev/null 2>&1 || $(GO) install golang.org/x/tools/cmd/deadcode@latest 2>/dev/null
	@printf "\n\033[1m[1/4] staticcheck\033[0m\n"
	@if ! command -v staticcheck >/dev/null 2>&1; then \
	  printf "  \033[33m⚠ staticcheck not found (install failed) — run: go install honnef.co/go/tools/cmd/staticcheck@latest\033[0m\n"; \
	else \
	  hits=$$(staticcheck ./... 2>&1 | tee /tmp/neo-audit-sc.txt | wc -l); \
	  if [ "$$hits" -eq 0 ]; then printf "  \033[32m✓ 0 findings\033[0m\n"; \
	  else printf "  \033[31m✗ %s findings\033[0m (see /tmp/neo-audit-sc.txt)\n" "$$hits"; head -5 /tmp/neo-audit-sc.txt | sed 's/^/    /'; fi; \
	fi
	@printf "\n\033[1m[2/4] ineffassign\033[0m\n"
	@if ! command -v ineffassign >/dev/null 2>&1; then \
	  printf "  \033[33m⚠ ineffassign not found (install failed) — run: go install github.com/gordonklaus/ineffassign@latest\033[0m\n"; \
	else \
	  hits=$$(ineffassign ./... 2>&1 | tee /tmp/neo-audit-ia.txt | grep -v "^$$" | wc -l); \
	  if [ "$$hits" -eq 0 ]; then printf "  \033[32m✓ 0 findings\033[0m\n"; \
	  else printf "  \033[31m✗ %s findings\033[0m\n" "$$hits"; head -5 /tmp/neo-audit-ia.txt | sed 's/^/    /'; fi; \
	fi
	@printf "\n\033[1m[3/4] modernize (Go 1.22+ idioms)\033[0m\n"
	@if ! command -v modernize >/dev/null 2>&1; then \
	  printf "  \033[33m⚠ modernize not found (install failed) — run: go install golang.org/x/tools/gopls/internal/analysis/modernize/cmd/modernize@latest\033[0m\n"; \
	else \
	  hits=$$(modernize ./... 2>&1 | grep -v "node_modules" | tee /tmp/neo-audit-mod.txt | grep -v "^$$" | wc -l); \
	  if [ "$$hits" -eq 0 ]; then printf "  \033[32m✓ 0 hints\033[0m\n"; \
	  else printf "  \033[33m⚠ %s hints\033[0m — run \`modernize -fix ./...\` to auto-apply\n" "$$hits"; fi; \
	fi
	@printf "\n\033[1m[4/4] test coverage\033[0m\n"
	@$(GO) test -short -cover ./pkg/... 2>&1 | awk '\
		/coverage:/ { \
			pkg = $$2; pct_str = $$5; gsub(/%/, "", pct_str); pct = pct_str + 0; \
			if (pct == 0) { col = "\033[31m"; } \
			else if (pct < 30) { col = "\033[33m"; } \
			else { col = "\033[32m"; } \
			printf "  %s%5.1f%%\033[0m  %s\n", col, pct, pkg; \
		} \
		/^ok/ && !/coverage:/ { printf "  \033[90m[no tests]\033[0m  %s\n", $$2 } \
	'
	@printf "\n\033[1mAudit complete.\033[0m Artifacts in /tmp/neo-audit-*.txt\n"

# audit-baseline: capture the current audit output as the known-good
# baseline against which `audit-ci` will diff. Run once after a clean
# landing; then `audit-ci` fails if any category shows a NEW finding.
# [Épica 237.C]
audit-baseline:
	@printf "\033[36m[make]\033[0m capturing audit baseline → .neo/audit-baseline.txt\n"
	@mkdir -p .neo
	@{ \
	  printf "# NeoAnvil audit baseline — regenerated by \`make audit-baseline\`\n"; \
	  printf "# Lines: staticcheck | ineffassign | modernize\n"; \
	  printf "# Regenerate after a clean landing; CI gate lives in \`make audit-ci\`.\n\n"; \
	  printf "=== staticcheck ===\n"; \
	  staticcheck ./... 2>&1 | sort; \
	  printf "\n=== ineffassign ===\n"; \
	  ineffassign ./... 2>&1 | grep -v '^$$' | sort; \
	  printf "\n=== modernize ===\n"; \
	  modernize ./... 2>&1 | grep -v "node_modules" | grep -v '^$$' | sort; \
	} > .neo/audit-baseline.txt
	@lines=$$(wc -l < .neo/audit-baseline.txt); \
	printf "  baseline: %s lines captured\n" "$$lines"

# audit-ci: fail-on-new. Runs the same three linters as `audit`, compares
# to .neo/audit-baseline.txt, and exits non-zero when ANY new line appears.
# Intended for CI: `make audit-ci` as a required step. [Épica 237.C]
audit-ci:
	@command -v staticcheck >/dev/null 2>&1 || $(GO) install honnef.co/go/tools/cmd/staticcheck@latest 2>/dev/null
	@command -v ineffassign >/dev/null 2>&1 || $(GO) install github.com/gordonklaus/ineffassign@latest 2>/dev/null
	@command -v modernize >/dev/null 2>&1 || $(GO) install golang.org/x/tools/gopls/internal/analysis/modernize/cmd/modernize@latest 2>/dev/null
	@if [ ! -f .neo/audit-baseline.txt ]; then \
	  printf "\033[31m[audit-ci] .neo/audit-baseline.txt missing — run \`make audit-baseline\` first\033[0m\n"; \
	  exit 2; \
	fi
	@current=$$(mktemp); \
	{ \
	  printf "# NeoAnvil audit baseline — regenerated by \`make audit-baseline\`\n"; \
	  printf "# Lines: staticcheck | ineffassign | modernize\n"; \
	  printf "# Regenerate after a clean landing; CI gate lives in \`make audit-ci\`.\n\n"; \
	  printf "=== staticcheck ===\n"; \
	  staticcheck ./... 2>&1 | sort; \
	  printf "\n=== ineffassign ===\n"; \
	  ineffassign ./... 2>&1 | grep -v '^$$' | sort; \
	  printf "\n=== modernize ===\n"; \
	  modernize ./... 2>&1 | grep -v "node_modules" | grep -v '^$$' | sort; \
	} > $$current; \
	diff_lines=$$(diff .neo/audit-baseline.txt $$current | grep -c '^>' || true); \
	if [ "$$diff_lines" = "0" ]; then \
	  printf "\033[32m[audit-ci] ✓ 0 NEW findings vs baseline\033[0m\n"; \
	  rm -f $$current; \
	  exit 0; \
	fi; \
	printf "\033[31m[audit-ci] ✗ %s NEW finding(s) vs baseline\033[0m\n" "$$diff_lines"; \
	printf "\n"; \
	diff .neo/audit-baseline.txt $$current | grep '^>' | head -20; \
	rm -f $$current; \
	exit 1

# freshness: report whether the running binaries are stale vs the latest
# commit touching cmd/ or pkg/. Useful in CI/CD before declaring a deploy
# successful, and locally as a one-shot answer to "did rebuild-restart catch
# my latest commit?". [SRE-107.C]
freshness:
	@for bin in $(MCP_BIN) $(NEXUS_BIN) $(CLI_BIN); do \
		if [ ! -f "$$bin" ]; then \
			echo "[freshness] $$bin: not built"; \
			continue; \
		fi; \
		bin_mtime=$$(python3 -c "import os; print(int(os.path.getmtime('$$bin')))" 2>/dev/null); \
		commit_ts=$$(git log -1 --format=%ct -- cmd/ pkg/ 2>/dev/null); \
		if [ -z "$$commit_ts" ]; then \
			echo "[freshness] $$bin: no git history for cmd/ pkg/"; \
			continue; \
		fi; \
		if [ "$$commit_ts" -gt "$$bin_mtime" ]; then \
			delta=$$(( (commit_ts - bin_mtime) / 60 )); \
			echo "[freshness] ⚠️  $$bin is stale by $${delta}m (last cmd|pkg commit newer)"; \
		else \
			delta=$$(( (bin_mtime - commit_ts) / 60 )); \
			echo "[freshness] ✓ $$bin is $${delta}m ahead of last cmd|pkg commit"; \
		fi; \
	done
