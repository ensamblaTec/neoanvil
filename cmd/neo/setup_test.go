package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestSetup_Scaffolds_NeoYaml_And_McpJson exercises the happy path:
// fresh tempdir, no existing neo.yaml, --bare disabled. Verifies that
// the rendered YAML is valid and the .mcp.json is well-formed JSON
// pointing at the canonical SSE URL with the workspace ID embedded.
func TestSetup_Scaffolds_NeoYaml_And_McpJson(t *testing.T) {
	tmp := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}

	opts := &setupOptions{
		WorkspaceName: "demo",
		Bare:          false,
		WithOllama:    false,
		Yes:           false,
	}
	if err := runSetup(opts); err != nil {
		// Ports might be in use during CI; non-fatal warning path
		// turns this into a real error only with --yes. Without --yes
		// the warnings shouldn't fail the run.
		if !strings.Contains(err.Error(), "already initialized") {
			t.Fatalf("runSetup: %v", err)
		}
	}

	// neo.yaml: parses as YAML and has expected scalars.
	yamlPath := filepath.Join(tmp, "demo", "neo.yaml")
	raw, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatalf("read %s: %v", yamlPath, err)
	}
	var parsed map[string]any
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("yaml parse: %v\n%s", err, raw)
	}
	server, _ := parsed["server"].(map[string]any)
	if server == nil {
		t.Fatalf("neo.yaml missing `server` section")
	}
	if got, want := server["nexus_dispatcher_port"], 9000; got != want {
		t.Errorf("dispatcher port = %v, want %v", got, want)
	}
	if got, want := server["host"], "127.0.0.1"; got != want {
		t.Errorf("host = %v, want %v", got, want)
	}

	// .mcp.json: valid JSON, contains workspace ID in the URL.
	mcpPath := filepath.Join(tmp, "demo", ".mcp.json")
	rawMCP, err := os.ReadFile(mcpPath)
	if err != nil {
		t.Fatalf("read %s: %v", mcpPath, err)
	}
	var mcp struct {
		MCPServers map[string]struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(rawMCP, &mcp); err != nil {
		t.Fatalf("mcp json parse: %v", err)
	}
	srv, ok := mcp.MCPServers["neoanvil"]
	if !ok {
		t.Fatalf(".mcp.json missing neoanvil server")
	}
	if !strings.Contains(srv.URL, "demo-") {
		t.Errorf("URL %q missing workspace ID prefix `demo-`", srv.URL)
	}
	if !strings.HasSuffix(srv.URL, "/mcp/sse") {
		t.Errorf("URL %q missing /mcp/sse suffix", srv.URL)
	}
}

// TestSetup_Bare_SkipsMcpJson verifies --bare omits .mcp.json (only
// neo.yaml is written).
func TestSetup_Bare_SkipsMcpJson(t *testing.T) {
	tmp := t.TempDir()
	cwd, _ := os.Getwd()
	defer os.Chdir(cwd)
	_ = os.Chdir(tmp)

	opts := &setupOptions{WorkspaceName: "bare-ws", Bare: true}
	if err := runSetup(opts); err != nil {
		if !strings.Contains(err.Error(), "already initialized") {
			t.Fatalf("runSetup: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(tmp, "bare-ws", "neo.yaml")); err != nil {
		t.Errorf("neo.yaml missing in bare mode: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "bare-ws", ".mcp.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf(".mcp.json should NOT exist in bare mode (err=%v)", err)
	}
}

// TestSetup_Docker_BindAddrAndUrlTemplate verifies --docker flips
// bind_addr to 0.0.0.0 and the .mcp.json URL becomes ${NEO_MCP_URL}.
func TestSetup_Docker_BindAddrAndUrlTemplate(t *testing.T) {
	tmp := t.TempDir()
	cwd, _ := os.Getwd()
	defer os.Chdir(cwd)
	_ = os.Chdir(tmp)

	opts := &setupOptions{WorkspaceName: "docker-ws", Docker: true}
	if err := runSetup(opts); err != nil {
		t.Fatalf("runSetup: %v", err)
	}
	yamlRaw, _ := os.ReadFile(filepath.Join(tmp, "docker-ws", "neo.yaml"))
	if !strings.Contains(string(yamlRaw), "host: 0.0.0.0") {
		t.Errorf("expected host: 0.0.0.0 in docker mode, got:\n%s", yamlRaw)
	}
	mcpRaw, _ := os.ReadFile(filepath.Join(tmp, "docker-ws", ".mcp.json"))
	if !strings.Contains(string(mcpRaw), "${NEO_MCP_URL}") {
		t.Errorf("expected ${NEO_MCP_URL} placeholder in docker mode, got:\n%s", mcpRaw)
	}
}

// TestSetup_RefuseExisting verifies pre-existing neo.yaml causes a
// fail-fast error rather than silent overwrite.
func TestSetup_RefuseExisting(t *testing.T) {
	tmp := t.TempDir()
	cwd, _ := os.Getwd()
	defer os.Chdir(cwd)
	_ = os.Chdir(tmp)

	target := filepath.Join(tmp, "exists")
	_ = os.MkdirAll(target, 0o755)
	if err := os.WriteFile(filepath.Join(target, "neo.yaml"), []byte("# pre-existing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := &setupOptions{WorkspaceName: "exists"}
	if err := runSetup(opts); err == nil || !strings.Contains(err.Error(), "already initialized") {
		t.Fatalf("expected refusal, got: %v", err)
	}
}

// TestWorkspaceIDFromName verifies the deterministic 5-char suffix
// derivation: same name produces same ID, different names differ.
func TestWorkspaceIDFromName(t *testing.T) {
	a := workspaceIDFromName("foo")
	b := workspaceIDFromName("foo")
	c := workspaceIDFromName("bar")
	if a != b {
		t.Errorf("non-deterministic: %q != %q", a, b)
	}
	if a == c {
		t.Errorf("collision: foo and bar produced same id %q", a)
	}
	if !strings.HasPrefix(a, "foo-") || len(a) != len("foo-")+5 {
		t.Errorf("malformed id %q", a)
	}
}

// TestSetup_RejectsPathTraversal verifies the regex sanitizer
// refuses workspace names that would escape the cwd via separator or
// dotdot. [DS-AUDIT 1.2 Finding 1]
func TestSetup_RejectsPathTraversal(t *testing.T) {
	tmp := t.TempDir()
	cwd, _ := os.Getwd()
	defer os.Chdir(cwd)
	_ = os.Chdir(tmp)

	bads := []string{
		"../etc",
		"../../evil",
		"foo/bar",
		"foo\\bar",
		".hidden",
		"-leading-hyphen",
		"with space",
		"with\nnewline",
		`name"with"quotes`,
	}
	for _, bad := range bads {
		opts := &setupOptions{WorkspaceName: bad}
		err := runSetup(opts)
		if err == nil || !strings.Contains(err.Error(), "invalid workspace name") {
			t.Errorf("name %q: expected invalid, got %v", bad, err)
		}
	}
}

// TestSetup_RejectsTOCTOU_Symlink verifies that an attacker-placed
// symlink at the destination path causes the write to fail rather
// than dereference into a sensitive file. [DS-AUDIT 1.2 Finding 3]
func TestSetup_RejectsTOCTOU_Symlink(t *testing.T) {
	tmp := t.TempDir()
	cwd, _ := os.Getwd()
	defer os.Chdir(cwd)
	_ = os.Chdir(tmp)

	wsDir := filepath.Join(tmp, "victim")
	_ = os.MkdirAll(wsDir, 0o755)

	// Plant a symlink at the path validateExisting will check.
	target := filepath.Join(tmp, "sensitive.txt")
	_ = os.WriteFile(target, []byte("DO NOT OVERWRITE\n"), 0o644)
	if err := os.Symlink(target, filepath.Join(wsDir, "neo.yaml")); err != nil {
		t.Skip("symlink unsupported on this fs:", err)
	}

	opts := &setupOptions{WorkspaceName: "victim"}
	err := runSetup(opts)
	if err == nil {
		t.Fatalf("expected refusal due to existing symlink, got nil")
	}
	// Verify the sensitive file was NOT touched.
	got, _ := os.ReadFile(target)
	if string(got) != "DO NOT OVERWRITE\n" {
		t.Errorf("sensitive file overwritten:\n%s", got)
	}
}

// TestSetup_UrlInjectionEscaped verifies a malicious --url value
// containing JSON metacharacters is escaped via json.Marshal rather
// than templated raw. [DS-AUDIT 1.2 Finding 2]
func TestSetup_UrlInjectionEscaped(t *testing.T) {
	tmp := t.TempDir()
	cwd, _ := os.Getwd()
	defer os.Chdir(cwd)
	_ = os.Chdir(tmp)

	bad := `http://evil.com"}, "evil": {"x": "y`
	opts := &setupOptions{WorkspaceName: "ws", MCPURL: bad}
	if err := runSetup(opts); err != nil {
		t.Fatalf("runSetup: %v", err)
	}
	mcpRaw, _ := os.ReadFile(filepath.Join(tmp, "ws", ".mcp.json"))

	// Result must still be valid JSON with exactly ONE server entry.
	var parsed struct {
		MCPServers map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(mcpRaw, &parsed); err != nil {
		t.Fatalf("malformed JSON after url injection attempt: %v\n%s", err, mcpRaw)
	}
	if len(parsed.MCPServers) != 1 {
		t.Errorf("injected extra server entry: got %d servers, want 1", len(parsed.MCPServers))
	}
	if _, ok := parsed.MCPServers["evil"]; ok {
		t.Errorf("attacker key 'evil' present in mcpServers")
	}
}

// TestCmpVersions verifies the dotted-version comparison handles
// suffixed (rc/beta) variants and missing components.
func TestCmpVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.26.0", "1.26.0", 0},
		{"1.26.1", "1.26.0", 1},
		{"1.26", "1.26.0", 0},
		{"1.25", "1.26", -1},
		{"1.26.1-rc1", "1.26.1", 0},
		{"2.0", "1.99", 1},
	}
	for _, c := range cases {
		if got := cmpVersions(c.a, c.b); got != c.want {
			t.Errorf("cmpVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
