//go:build mobile

package mobile

import "testing"

// TestBrainNodeID_Deterministic — same input must always produce the
// same NodeID across invocations and across reboots. This is the
// invariant federation peers rely on to detect "this is my own snapshot
// reflected back" and short-circuit no-op restores.
func TestBrainNodeID_Deterministic(t *testing.T) {
	const input = "android-id-1234567890abcdef"
	a := BrainNodeID(input)
	b := BrainNodeID(input)
	if a != b {
		t.Errorf("BrainNodeID is not deterministic: a=%q b=%q", a, b)
	}
	if !startsWith(a, "node:") {
		t.Errorf("BrainNodeID output missing 'node:' prefix: %q", a)
	}
	if len(a) != len("node:")+16 {
		t.Errorf("BrainNodeID output length = %d, want %d", len(a), len("node:")+16)
	}
}

// TestBrainNodeID_DifferentInputsDiverge — different ANDROID_IDs MUST
// produce different NodeIDs. Otherwise multiple Pixel devices would
// share an identity and federation would silently merge their state.
func TestBrainNodeID_DifferentInputsDiverge(t *testing.T) {
	a := BrainNodeID("device-A")
	b := BrainNodeID("device-B")
	if a == b {
		t.Errorf("two distinct inputs collapse to the same NodeID: %q", a)
	}
}

// TestBrainNodeID_EmptyInputFallback — empty input MUST NOT panic; it
// returns a sentinel value the receiver can recognise.
func TestBrainNodeID_EmptyInputFallback(t *testing.T) {
	got := BrainNodeID("")
	if got != "node:mobile-unknown" {
		t.Errorf("BrainNodeID('') = %q, want 'node:mobile-unknown'", got)
	}
}

// TestBrainNodeFingerprint_DeterministicAndDistinct — same property
// pair as BrainNodeID, separately tested because the two derive from
// different domain-separated SHA-256 inputs and must NOT collide.
func TestBrainNodeFingerprint_DeterministicAndDistinct(t *testing.T) {
	a1 := BrainNodeFingerprint("install-uuid-1")
	a2 := BrainNodeFingerprint("install-uuid-1")
	b := BrainNodeFingerprint("install-uuid-2")
	if a1 != a2 {
		t.Errorf("fingerprint not deterministic: a1=%q a2=%q", a1, a2)
	}
	if a1 == b {
		t.Errorf("distinct inputs produced identical fingerprints: %q", a1)
	}
	// Cross-check: same input to NodeID and Fingerprint must NOT collide.
	// They use different domain-separator strings inside SHA-256.
	if BrainNodeID("install-uuid-1") == a1 {
		t.Errorf("NodeID and Fingerprint collide for the same input — domain separation broken")
	}
}

// TestBrainNodeFingerprint_EmptyFallback — empty input returns the
// degenerate-but-well-formed sentinel.
func TestBrainNodeFingerprint_EmptyFallback(t *testing.T) {
	got := BrainNodeFingerprint("")
	if got != "node:mobile-anon" {
		t.Errorf("BrainNodeFingerprint('') = %q, want 'node:mobile-anon'", got)
	}
}

// TestLoadConfigBytes_HappyPath — round-trip a YAML blob through
// LoadConfigBytes and verify the field mapping. This is the primary
// hot path for the Android side: read assets/neo.yaml.gz → decompress
// → LoadConfigBytes → use the *MobileConfig in the Sync screen.
func TestLoadConfigBytes_HappyPath(t *testing.T) {
	yamlBytes := []byte(`
bucket_url: https://r2.example.com/brain
bucket_access_key: AK_TEST
bucket_secret_key: SK_TEST
passphrase_hint: pets+lunch
peers:
  - mac-pro
  - desktop-linux
power_policy: charging
node_id: persisted-uuid-abc
node_fingerprint: persisted-fp-xyz
`)
	cfg, err := LoadConfigBytes(yamlBytes)
	if err != nil {
		t.Fatalf("LoadConfigBytes: %v", err)
	}
	if cfg.BucketURL != "https://r2.example.com/brain" {
		t.Errorf("BucketURL = %q", cfg.BucketURL)
	}
	if cfg.BucketAccessKey != "AK_TEST" || cfg.BucketSecretKey != "SK_TEST" {
		t.Errorf("creds round-trip failed: %+v", cfg)
	}
	if len(cfg.Peers) != 2 || cfg.Peers[0] != "mac-pro" || cfg.Peers[1] != "desktop-linux" {
		t.Errorf("peers list mismatch: %+v", cfg.Peers)
	}
	if cfg.PowerPolicy != "charging" {
		t.Errorf("PowerPolicy = %q", cfg.PowerPolicy)
	}
	if cfg.NodeID != "persisted-uuid-abc" || cfg.NodeFingerprint != "persisted-fp-xyz" {
		t.Errorf("identity overrides failed: %+v", cfg)
	}
}

// TestLoadConfigBytes_RejectsEmpty — empty input is a programmer error
// (e.g. asset not found); fail loudly rather than returning a blank
// MobileConfig that the UI might surface as if everything were fine.
func TestLoadConfigBytes_RejectsEmpty(t *testing.T) {
	if _, err := LoadConfigBytes(nil); err == nil {
		t.Error("LoadConfigBytes(nil) should error")
	}
	if _, err := LoadConfigBytes([]byte{}); err == nil {
		t.Error("LoadConfigBytes([]) should error")
	}
}

// TestConfigToYAMLBytes_RoundTrip — Marshal → Unmarshal must preserve
// every field. Used when the Android UI persists user-edited config
// back to disk.
func TestConfigToYAMLBytes_RoundTrip(t *testing.T) {
	original := &MobileConfig{
		BucketURL:       "https://r2.example.com",
		Peers:           []string{"mac"},
		PowerPolicy:     "wifi",
		NodeID:          "id1",
		NodeFingerprint: "fp1",
	}
	out, err := ConfigToYAMLBytes(original)
	if err != nil {
		t.Fatalf("ConfigToYAMLBytes: %v", err)
	}
	roundTripped, err := LoadConfigBytes(out)
	if err != nil {
		t.Fatalf("LoadConfigBytes(round trip): %v", err)
	}
	if roundTripped.BucketURL != original.BucketURL ||
		roundTripped.PowerPolicy != original.PowerPolicy ||
		roundTripped.NodeID != original.NodeID ||
		roundTripped.NodeFingerprint != original.NodeFingerprint ||
		len(roundTripped.Peers) != 1 || roundTripped.Peers[0] != "mac" {
		t.Errorf("round-trip mismatch: orig=%+v got=%+v", original, roundTripped)
	}
}

// startsWith is a stdlib-free prefix check kept here so the test file
// has zero non-stdlib imports — keeps gomobile bind happy.
func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
