// Package mobile is the gomobile-safe wrapper layer for NeoAnvil's brain
// peer functionality on Android (and, eventually, iOS via the same `gomobile
// bind` machinery). PILAR XXVI 137.A.2.
//
// Build tag:
//
//	//go:build mobile
//
// Files in this package compile only when the caller passes -tags=mobile.
// `go build ./...` from the repo root SKIPS this package — Linux production
// code is unaffected.
//
// What lives here, in one sentence:
//
//	A minimal API surface that lets a JVM-hosted Android process (a) push a
//	brain snapshot to R2 / tsnet, (b) pull and decrypt a peer's snapshot,
//	(c) load a config blob from Android assets, and (d) supply identity +
//	fingerprint values that originate on the Android side (ANDROID_ID, a
//	per-install UUID).
//
// What does NOT live here (by design):
//
//   - pkg/rag — no HNSW, no semantic search on the mobile node. The Pixel
//     is a brain peer, not a full neo-mcp instance.
//   - pkg/sre — no thermal / scaling / healer subsystems. Android exposes
//     battery + thermal via the BatteryManager / ThermalService APIs that
//     the Kotlin layer reads directly (see 137.C.3).
//   - cmd/neo-mcp — no MCP server runs on mobile. The Pixel runs neo-mcp
//     "lite" via tsnet (137.E.1) only after 137.B/C/D/E/F land.
//   - cmd/neo-nexus — no dispatcher.
//
// Wrappers replace exec / sysfs paths from the production code:
//
//	pkg/brain/identity.go::ResolveCanonicalID — uses os/exec to run `git`
//	    Linux-only; mobile/ provides BrainNodeID(provided string) which
//	    accepts the canonical ID from the JNI layer (Android passes
//	    Settings.Secure.ANDROID_ID or a persisted UUID).
//
//	pkg/brain/manifest.go::NodeFingerprint — reads /etc/machine-id
//	    Blocked by SELinux untrusted_app domain on Android; mobile/
//	    provides BrainNodeFingerprint(provided string) which accepts a
//	    stable per-install UUID from the JNI layer.
//
// Audit detail: docs/pilar-xxvi-gomobile-compat-audit.md (137.A.1).
//
// Build commands (post-137.A.3 once gomobile is installed):
//
//	go install golang.org/x/mobile/cmd/gomobile@latest
//	gomobile init
//	gomobile bind -target=android -tags=mobile -o mobile/build/libneo.aar ./mobile
//
// Until 137.A.3 lands, this package is verified by:
//
//	go build -tags mobile ./mobile
//
// which exercises the gomobile-safe code paths from a vanilla Linux toolchain.
package mobile
