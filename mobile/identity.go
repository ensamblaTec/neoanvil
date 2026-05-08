//go:build mobile

package mobile

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/ensamblatec/neoanvil/pkg/brain"
)

// BrainNodeID returns the canonical NodeID to embed in a manifest produced
// by the mobile node.
//
// On Linux, pkg/brain/identity.go::ResolveCanonicalID walks `.git/config`,
// runs `git remote -v` via os/exec, reads `neo.yaml::workspace.canonical_id`,
// and finally falls back to a hash of the absolute path. Android cannot
// run git subprocesses (Android's SELinux untrusted_app domain blocks
// fork+exec), so this wrapper takes the canonical identifier from the
// JNI layer instead.
//
// The Android side (Kotlin/Jetpack Compose, 137.B.3) supplies one of:
//
//  1. Settings.Secure.ANDROID_ID — 64-bit value reset on factory reset,
//     stable across reboots, scoped to (app + signing key + user). Good
//     enough for a single-user dev device.
//  2. A SecureRandom UUID stored in EncryptedSharedPreferences on first
//     launch and read on subsequent launches. Survives app upgrades but
//     is wiped on app uninstall.
//
// Both are passed verbatim as the `provided` argument. If `provided` is
// empty, BrainNodeID returns "node:mobile-unknown" so the manifest is
// always well-formed (matches the SourcePathHash fallback in the Linux
// implementation).
//
// The format mirrors pkg/brain/manifest.go::NodeFingerprint output: a
// "node:" prefix plus the lowercase hex of sha256(provided) truncated to
// 16 chars. Two devices with the same `provided` value resolve to the
// same NodeID — intentional, supports Pixel→Mac federation when the user
// configures the same identity on both ends.
func BrainNodeID(provided string) string {
	provided = strings.TrimSpace(provided)
	if provided == "" {
		return "node:mobile-unknown"
	}
	sum := sha256.Sum256([]byte("brain:mobile:" + provided))
	return "node:" + hex.EncodeToString(sum[:8])
}

// BrainNodeFingerprint returns the per-machine fingerprint used as the
// salt input to brain.DeriveKey. PILAR XXVI 137.A.2.
//
// pkg/brain/manifest.go::NodeFingerprint reads /etc/machine-id and
// /var/lib/dbus/machine-id on Linux. Both paths are blocked on Android.
// This wrapper takes the fingerprint from the JNI layer (typically a
// SecureRandom 16-byte value persisted to EncryptedSharedPreferences on
// first run, then constant for the lifetime of the install).
//
// The function is intentionally permissive: an empty `provided` falls
// back to a hostname-derived fingerprint. On Android, os.Hostname()
// returns "localhost", so the empty-input fallback is a degenerate case
// (every Android device that omits the JNI value ends up with the same
// fingerprint). Callers MUST supply a non-empty value in production.
//
// The output format matches the Linux implementation exactly so that
// archives produced on Android decrypt correctly when pulled from a
// Mac/Linux peer using the same passphrase.
func BrainNodeFingerprint(provided string) string {
	provided = strings.TrimSpace(provided)
	if provided == "" {
		// Last-ditch fallback. Production callers MUST pass a real value
		// from the Android side; this branch exists so an early-bring-up
		// JNI bridge with provided=="" still produces a well-formed
		// manifest rather than panicking.
		return "node:mobile-anon"
	}
	sum := sha256.Sum256([]byte("brain:mobile-fingerprint:" + provided))
	return "node:" + hex.EncodeToString(sum[:8])
}

// brainResolutionFromMobile constructs the Resolution shape the rest of
// pkg/brain expects, marking it as a mobile-supplied identity. Used by
// BrainPush / BrainPull to short-circuit the exec-based resolution chain.
//
// We deliberately reuse the Linux SourceConfigOverride enum value rather
// than introducing a "SourceMobile" constant — the receiver of the
// archive does not need to know whether the identity was provided by JNI
// or by neo.yaml::workspace.canonical_id; both are operator-attested
// strings. Adding a new enum value would force every Linux peer to
// recognise it on receive.
//
//nolint:unused // Reserved for BrainPush / BrainPull wiring (next sub-épica). Keeps the API surface coherent with pkg/brain.identity.go without import-cycle hacks.
func brainResolutionFromMobile(provided string) brain.Resolution {
	return brain.Resolution{
		ID:     BrainNodeID(provided),
		Source: brain.SourceConfigOverride,
	}
}
