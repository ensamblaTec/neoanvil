#!/usr/bin/env bash
# PILAR XXVI 137.A.3 — gomobile bind script for Android.
#
# Produces mobile/build/libneo.aar from the //go:build mobile-tagged
# wrappers in ./mobile (137.A.2). The Android Studio project under
# mobile/android/ (137.B.*) imports this AAR via Gradle.
#
# Prerequisites (none of these are Linux-only — script is portable to
# macOS dev machines too):
#   - gomobile binary in PATH:
#       go install golang.org/x/mobile/cmd/gomobile@latest
#   - ANDROID_SDK_ROOT (or ANDROID_HOME) pointing to an Android SDK install
#     with NDK + platform-tools + cmdline-tools. Easiest path:
#       1. install Android Studio
#       2. Tools → SDK Manager → install "NDK (Side by side)" and a
#          platform >= API 34 (Android 14, the gomobile target)
#       3. export ANDROID_SDK_ROOT="$HOME/Android/Sdk"   # Linux default
#                                or "$HOME/Library/Android/sdk"  # macOS
#   - JDK 17+ on PATH (gomobile uses gradle internally for the AAR step)
#
# Modes:
#   --check         only validate prerequisites, do not invoke gomobile
#   --init          run `gomobile init` (downloads + verifies NDK pieces)
#   (no flag)       full build: produces mobile/build/libneo.aar
#
# Output:
#   mobile/build/libneo.aar          — the AAR consumed by Android Studio
#   mobile/build/libneo-sources.jar  — source jar (debugger support)
#
# Exit codes:
#   0 success / all checks pass
#   1 missing prerequisite (gomobile or SDK)
#   2 gomobile bind failed
#
# CI-friendly: pass --check from a pre-merge hook to verify the dev
# machine is wired up before reviewers rebase.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MOBILE_DIR="${REPO_ROOT}/mobile"
BUILD_DIR="${MOBILE_DIR}/build"
AAR_OUT="${BUILD_DIR}/libneo.aar"

log() { printf "[build-android] %s\n" "$*"; }
fail() { printf "[build-android] ERROR: %s\n" "$*" >&2; exit "${2:-1}"; }

check_prereqs() {
  log "checking gomobile binary..."
  if ! command -v gomobile >/dev/null 2>&1; then
    fail "gomobile not in PATH. Install with: go install golang.org/x/mobile/cmd/gomobile@latest"
  fi
  log "  gomobile: $(command -v gomobile)"

  log "checking ANDROID_SDK_ROOT (or ANDROID_HOME)..."
  local sdk="${ANDROID_SDK_ROOT:-${ANDROID_HOME:-}}"
  if [[ -z "$sdk" ]]; then
    fail "ANDROID_SDK_ROOT and ANDROID_HOME both unset. Install Android Studio + SDK and export ANDROID_SDK_ROOT=\"\$HOME/Android/Sdk\"."
  fi
  if [[ ! -d "$sdk" ]]; then
    fail "ANDROID_SDK_ROOT points to non-existent dir: $sdk"
  fi
  log "  SDK root: $sdk"

  if [[ ! -d "$sdk/ndk" ]] && [[ ! -d "$sdk/ndk-bundle" ]]; then
    fail "no NDK found under $sdk. Open Android Studio → SDK Manager → install 'NDK (Side by side)'."
  fi

  log "checking JDK..."
  if ! command -v java >/dev/null 2>&1; then
    fail "java not in PATH. Install JDK 17+ (sudo apt install default-jdk on Debian/Ubuntu)."
  fi
  local jver
  jver="$(java -version 2>&1 | head -1)"
  log "  $jver"

  log "checking //go:build mobile package compiles..."
  ( cd "$REPO_ROOT" && go build -tags mobile ./mobile/ ) || fail "go build -tags mobile failed; fix the package before binding"
  log "  ./mobile/ compiles cleanly"

  log "all prerequisites satisfied."
}

run_init() {
  log "running gomobile init (this can download ~1GB of NDK pieces on first run)..."
  gomobile init || fail "gomobile init failed" 1
  log "gomobile init complete."
}

run_bind() {
  mkdir -p "$BUILD_DIR"
  log "running gomobile bind (target=android, tags=mobile, output=$AAR_OUT)..."
  ( cd "$REPO_ROOT" && gomobile bind \
      -target=android \
      -tags=mobile \
      -o "$AAR_OUT" \
      ./mobile ) || fail "gomobile bind failed — see stderr above" 2
  log "build complete: $AAR_OUT"
  log "  $(ls -lh "$AAR_OUT" | awk '{print $5, $9}')"
  if [[ -f "${AAR_OUT%.aar}-sources.jar" ]]; then
    log "  $(ls -lh "${AAR_OUT%.aar}-sources.jar" | awk '{print $5, $9}')"
  fi
}

case "${1:-}" in
  --check)
    check_prereqs
    log "(--check only; not building)"
    ;;
  --init)
    check_prereqs
    run_init
    ;;
  ""|--build)
    check_prereqs
    run_bind
    ;;
  *)
    cat <<EOF
usage: $0 [--check | --init | --build]

  --check  validate gomobile + Android SDK + JDK + that ./mobile compiles
  --init   run gomobile init (downloads NDK pieces if missing)
  --build  full bind → mobile/build/libneo.aar (default)

Run --check on a fresh dev machine to verify the env before --init / --build.
EOF
    exit 1
    ;;
esac
