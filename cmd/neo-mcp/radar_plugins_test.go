package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/config"
)

// newPluginStatusServer returns an httptest.Server that responds to
// /api/v1/plugins with the given JSON body and status. Helper for unit
// tests of handlePluginStatus.
func newPluginStatusServer(t *testing.T, status int, body string) (*httptest.Server, int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/plugins" {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)

	// httptest.Server URL is http://127.0.0.1:<port>; extract port.
	parts := strings.Split(strings.TrimPrefix(srv.URL, "http://"), ":")
	if len(parts) != 2 {
		t.Fatalf("unexpected srv URL: %s", srv.URL)
	}
	var port int
	if _, err := fmt.Sscanf(parts[1], "%d", &port); err != nil {
		t.Fatalf("parse port: %v", err)
	}
	return srv, port
}

func makeRadarToolForPluginsTest(t *testing.T, nexusPort int) *RadarTool {
	t.Helper()
	cfg := &config.NeoConfig{}
	cfg.Server.NexusDispatcherPort = nexusPort
	return &RadarTool{cfg: cfg}
}

// mcpTextOf extracts the text payload from the mcpText() envelope returned by
// handlePluginStatus: map[string]any{"content": []map[string]any{{"type":"text","text":...}}}.
// Radar handlers MUST return this MCP CallToolResult shape — a bare string is
// silently rejected by the MCP SDK and hangs the client.
func mcpTextOf(t *testing.T, out any) string {
	t.Helper()
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("expected mcpText envelope (map[string]any), got %T", out)
	}
	content, ok := m["content"].([]map[string]any)
	if !ok || len(content) == 0 {
		t.Fatalf("expected non-empty content array, got %#v", m["content"])
	}
	text, ok := content[0]["text"].(string)
	if !ok {
		t.Fatalf("expected text string in content[0], got %T", content[0]["text"])
	}
	return text
}

func TestHandlePluginStatus_DisabledRendersDisabled(t *testing.T) {
	body := `{"enabled":false,"reason":"nexus.plugins.enabled is false"}`
	_, port := newPluginStatusServer(t, http.StatusOK, body)
	rt := makeRadarToolForPluginsTest(t, port)

	out, err := rt.handlePluginStatus(context.Background(), nil)
	if err != nil {
		t.Fatalf("handlePluginStatus: %v", err)
	}
	md := mcpTextOf(t, out)
	if !strings.Contains(md, "disabled") {
		t.Errorf("disabled status not surfaced:\n%s", md)
	}
	if !strings.Contains(md, "nexus.plugins.enabled is false") {
		t.Errorf("reason missing:\n%s", md)
	}
}

func TestHandlePluginStatus_EnabledWithPlugins(t *testing.T) {
	body := `{
		"enabled": true,
		"manifest_version": 1,
		"plugins": [{"name":"jira","pid":12345,"status":"running"}],
		"tools": ["jira/get_context", "jira/transition"],
		"errors": {}
	}`
	_, port := newPluginStatusServer(t, http.StatusOK, body)
	rt := makeRadarToolForPluginsTest(t, port)

	out, err := rt.handlePluginStatus(context.Background(), nil)
	if err != nil {
		t.Fatalf("handlePluginStatus: %v", err)
	}
	md := mcpTextOf(t, out)

	mustContain := []string{
		"enabled — manifest_version=1",
		"`jira`",
		"12345",
		"running",
		"jira/get_context",
		"jira/transition",
		"Aggregated tools (2)",
	}
	for _, s := range mustContain {
		if !strings.Contains(md, s) {
			t.Errorf("output missing %q\n--- got ---\n%s", s, md)
		}
	}
}

func TestHandlePluginStatus_ErrorsSection(t *testing.T) {
	body := `{
		"enabled": true,
		"plugins": [],
		"tools": [],
		"errors": {"jira":"spawn: binary not found"}
	}`
	_, port := newPluginStatusServer(t, http.StatusOK, body)
	rt := makeRadarToolForPluginsTest(t, port)
	out, _ := rt.handlePluginStatus(context.Background(), nil)
	md := mcpTextOf(t, out)
	if !strings.Contains(md, "## Errors") {
		t.Errorf("errors section missing:\n%s", md)
	}
	if !strings.Contains(md, "spawn: binary not found") {
		t.Errorf("error detail missing:\n%s", md)
	}
}

func TestHandlePluginStatus_NexusUnreachable(t *testing.T) {
	// Use a port nothing listens on
	rt := makeRadarToolForPluginsTest(t, 1)

	out, err := rt.handlePluginStatus(context.Background(), nil)
	if err != nil {
		t.Errorf("handler should never return error (fail-soft): %v", err)
	}
	md := mcpTextOf(t, out)
	if !strings.Contains(md, "unreachable") {
		t.Errorf("unreachable status not surfaced:\n%s", md)
	}
}

func TestHandlePluginStatus_NexusReturns500(t *testing.T) {
	_, port := newPluginStatusServer(t, http.StatusInternalServerError, "boom")
	rt := makeRadarToolForPluginsTest(t, port)
	out, err := rt.handlePluginStatus(context.Background(), nil)
	if err != nil {
		t.Errorf("handler should never return error: %v", err)
	}
	md := mcpTextOf(t, out)
	if !strings.Contains(md, "Nexus returned 500") {
		t.Errorf("status code not surfaced:\n%s", md)
	}
}

func TestHandlePluginStatus_NexusReturnsBadJSON(t *testing.T) {
	_, port := newPluginStatusServer(t, http.StatusOK, "not json")
	rt := makeRadarToolForPluginsTest(t, port)
	out, err := rt.handlePluginStatus(context.Background(), nil)
	if err != nil {
		t.Errorf("handler should never return error: %v", err)
	}
	md := mcpTextOf(t, out)
	if !strings.Contains(md, "decode") {
		t.Errorf("decode error not surfaced:\n%s", md)
	}
}

func TestNexusBaseURL_EnvOverride(t *testing.T) {
	t.Setenv("NEO_NEXUS_URL", "http://example.test:4242")
	rt := makeRadarToolForPluginsTest(t, 9000)
	if got := nexusBaseURL(rt); got != "http://example.test:4242" {
		t.Errorf("env override ignored: got %q", got)
	}
}

func TestNexusBaseURL_ConfigPort(t *testing.T) {
	t.Setenv("NEO_NEXUS_URL", "")
	rt := makeRadarToolForPluginsTest(t, 9123)
	if got := nexusBaseURL(rt); got != "http://127.0.0.1:9123" {
		t.Errorf("config port ignored: got %q", got)
	}
}

func TestFetchPluginsSegment_Disabled(t *testing.T) {
	body := `{"enabled":false}`
	_, port := newPluginStatusServer(t, http.StatusOK, body)
	rt := makeRadarToolForPluginsTest(t, port)
	if got := fetchPluginsSegment(rt); got != "" {
		t.Errorf("disabled should return empty, got %q", got)
	}
}

func TestFetchPluginsSegment_ActiveNoErrors(t *testing.T) {
	body := `{
		"enabled": true,
		"plugins": [
			{"name":"jira","pid":1,"status":"running"},
			{"name":"github","pid":2,"status":"running"}
		],
		"errors": {}
	}`
	_, port := newPluginStatusServer(t, http.StatusOK, body)
	rt := makeRadarToolForPluginsTest(t, port)
	got := fetchPluginsSegment(rt)
	if !strings.HasPrefix(got, " | plugins: 2 active") {
		t.Errorf("expected ' | plugins: 2 active' prefix, got %q", got)
	}
	if !strings.Contains(got, "github") || !strings.Contains(got, "jira") {
		t.Errorf("plugin names missing: %q", got)
	}
}

func TestFetchPluginsSegment_PartialErrors(t *testing.T) {
	body := `{
		"enabled": true,
		"plugins": [{"name":"jira","pid":1,"status":"running"}],
		"errors": {"github":"spawn failed"}
	}`
	_, port := newPluginStatusServer(t, http.StatusOK, body)
	rt := makeRadarToolForPluginsTest(t, port)
	got := fetchPluginsSegment(rt)
	if !strings.HasPrefix(got, " | plugins: 1/2 errored") {
		t.Errorf("expected '1/2 errored', got %q", got)
	}
}

func TestFetchPluginsSegment_NoPluginsAndNoErrors(t *testing.T) {
	body := `{"enabled":true,"plugins":[],"errors":{}}`
	_, port := newPluginStatusServer(t, http.StatusOK, body)
	rt := makeRadarToolForPluginsTest(t, port)
	if got := fetchPluginsSegment(rt); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestFetchPluginsSegment_NexusUnreachable(t *testing.T) {
	rt := makeRadarToolForPluginsTest(t, 1)
	if got := fetchPluginsSegment(rt); got != "" {
		t.Errorf("unreachable should return empty (silent), got %q", got)
	}
}

func TestFetchPluginsSegment_NameTruncation(t *testing.T) {
	body := `{
		"enabled": true,
		"plugins": [
			{"name":"plugin-with-very-long-name-that-exceeds-budget","pid":1,"status":"running"},
			{"name":"another-very-long-plugin-name-here-also","pid":2,"status":"running"}
		]
	}`
	_, port := newPluginStatusServer(t, http.StatusOK, body)
	rt := makeRadarToolForPluginsTest(t, port)
	got := fetchPluginsSegment(rt)
	if !strings.Contains(got, "...") {
		t.Errorf("expected truncation marker, got %q", got)
	}
	// Combined display capped at 60 chars + ellipsis = 63 max in display segment
	if len(got) > 100 {
		t.Errorf("compact line segment too long (%d chars): %q", len(got), got)
	}
}

func TestNexusBaseURL_FallsBackToDefault(t *testing.T) {
	t.Setenv("NEO_NEXUS_URL", "")
	rt := &RadarTool{cfg: &config.NeoConfig{}} // port=0
	if got := nexusBaseURL(rt); got != "http://127.0.0.1:9000" {
		t.Errorf("default fallback wrong: got %q", got)
	}
}
