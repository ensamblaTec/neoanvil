package main

import (
	"testing"
)

func TestIsHotPath_Match(t *testing.T) {
	hotPaths := []string{"pkg/brain/crypto.go", "pkg/brain/storage/*.go", "pkg/auth/*.go"}
	ws := "/workspace"

	cases := []struct {
		file string
		want bool
	}{
		{"/workspace/pkg/brain/crypto.go", true},
		{"/workspace/pkg/brain/storage/local.go", true},
		{"/workspace/pkg/brain/storage/r2.go", true},
		{"/workspace/pkg/auth/keystore.go", true},
		{"/workspace/pkg/rag/hnsw.go", false},
		{"/workspace/cmd/neo-mcp/main.go", false},
		{"/workspace/pkg/brain/crypto_test.go", false},
	}
	for _, tc := range cases {
		got := isHotPath(tc.file, hotPaths, ws)
		if got != tc.want {
			t.Errorf("isHotPath(%q) = %v, want %v", tc.file, got, tc.want)
		}
	}
}

func TestIsHotPath_OutsideWorkspace(t *testing.T) {
	hotPaths := []string{"pkg/auth/*.go"}
	got := isHotPath("/other/pkg/auth/keystore.go", hotPaths, "/workspace")
	if got {
		t.Error("file outside workspace should not match")
	}
}

func TestIsHotPath_EmptyPatterns(t *testing.T) {
	got := isHotPath("/workspace/pkg/auth/keystore.go", nil, "/workspace")
	if got {
		t.Error("empty patterns should never match")
	}
}

func TestFormatDSCertifyCheck_Off(t *testing.T) {
	r := deepseekPreCheckResult{Mode: "off"}
	if got := formatDSCertifyCheck(r); got != "skipped" {
		t.Errorf("off mode should return 'skipped', got %q", got)
	}
}

func TestFormatDSCertifyCheck_NotHot(t *testing.T) {
	r := deepseekPreCheckResult{Mode: "manual", IsHot: false}
	if got := formatDSCertifyCheck(r); got != "skipped" {
		t.Errorf("non-hot should return 'skipped', got %q", got)
	}
}

func TestFormatDSCertifyCheck_ManualAdvisory(t *testing.T) {
	r := deepseekPreCheckResult{Mode: "manual", IsHot: true, Summary: "advisory text"}
	got := formatDSCertifyCheck(r)
	if got != "advisory:advisory text" {
		t.Errorf("manual hot should return advisory, got %q", got)
	}
}

func TestFormatDSCertifyCheck_Blocked(t *testing.T) {
	r := deepseekPreCheckResult{Mode: "auto", IsHot: true, Blocked: true, Summary: "SEV 9 finding"}
	got := formatDSCertifyCheck(r)
	if got != "fail:SEV 9 finding" {
		t.Errorf("blocked should return fail, got %q", got)
	}
}
