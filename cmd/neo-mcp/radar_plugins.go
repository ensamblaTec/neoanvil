// cmd/neo-mcp/radar_plugins.go — PLUGIN_STATUS radar intent.
// PILAR XXIII / Épica 126.5.
//
// Surfaces the subprocess plugin pool state to the agent without requiring
// the operator to curl /api/v1/plugins manually. Calls Nexus's plugin
// status endpoint (loopback HTTP via SafeInternalHTTPClient) and renders
// the result as Markdown for Claude.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/sre"
)

// pluginStatusResponse mirrors the JSON shape served by
// cmd/neo-nexus/plugin_boot.go::handlePluginsStatus. Defensive: only
// fields we actually render are decoded.
type pluginStatusResponse struct {
	Enabled         bool              `json:"enabled"`
	Reason          string            `json:"reason,omitempty"`
	Plugins         []pluginRunInfo   `json:"plugins,omitempty"`
	Tools           []string          `json:"tools,omitempty"`
	Errors          map[string]string `json:"errors,omitempty"`
	ManifestVersion int               `json:"manifest_version,omitempty"`
}

type pluginRunInfo struct {
	Name   string             `json:"name"`
	PID    int                `json:"pid"`
	Status string             `json:"status"`
	Health *pluginHealthBrief `json:"health,omitempty"` // [ÉPICA 152.C]
}

// pluginHealthBrief is the BRIEFING-relevant subset of the per-plugin
// __health__ snapshot. Decoded from the embedded "health" field in
// /api/v1/plugins. [ÉPICA 152.C]
type pluginHealthBrief struct {
	Alive            bool     `json:"alive"`
	ToolsRegistered  []string `json:"tools_registered"`
	LastDispatchUnix int64    `json:"last_dispatch_unix"`
	ErrorCount       int64    `json:"error_count"`
	PolledAtUnix     int64    `json:"polled_at_unix"`
	PollErr          string   `json:"poll_err"`
}

// IsZombie returns true when the plugin is process-alive but its
// dispatcher is non-functional. Three signals trigger zombie:
//   - poll_err != "" (the most recent __health__ call failed)
//   - tools_registered == [] (plugin handshake completed but no tools)
//   - polled_at_unix is older than 90s (poll loop broke or stdio hung)
func (h *pluginHealthBrief) IsZombie() bool {
	if h == nil {
		return false // no data yet — don't crash on missing
	}
	if h.PollErr != "" {
		return true
	}
	if len(h.ToolsRegistered) == 0 && h.PolledAtUnix > 0 {
		return true
	}
	// Stale poll: more than 3× the 30s poll interval means the monitor
	// stopped or the plugin's stdio hung mid-response.
	if h.PolledAtUnix > 0 && time.Now().Unix()-h.PolledAtUnix > 90 {
		return true
	}
	return false
}

// handlePluginStatus is the radar handler for PLUGIN_STATUS.
func (t *RadarTool) handlePluginStatus(ctx context.Context, _ map[string]any) (any, error) {
	url := nexusBaseURL(t) + "/api/v1/plugins"
	client := sre.SafeInternalHTTPClient(5)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return mcpText(renderPluginStatusError(fmt.Sprintf("contacting Nexus: %v", err))), nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return mcpText(renderPluginStatusError(fmt.Sprintf("Nexus returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body))))), nil
	}
	var status pluginStatusResponse
	if err := json.Unmarshal(body, &status); err != nil {
		return mcpText(renderPluginStatusError(fmt.Sprintf("decode response: %v", err))), nil
	}
	return mcpText(renderPluginStatusMarkdown(&status)), nil
}

// nexusBaseURL resolves the dispatcher URL: env override > config > default.
func nexusBaseURL(t *RadarTool) string {
	if v := strings.TrimSpace(os.Getenv("NEO_NEXUS_URL")); v != "" {
		return v
	}
	if t != nil && t.cfg != nil && t.cfg.Server.NexusDispatcherPort != 0 {
		return fmt.Sprintf("http://127.0.0.1:%d", t.cfg.Server.NexusDispatcherPort)
	}
	return "http://127.0.0.1:9000"
}

// renderPluginStatusMarkdown produces a compact, agent-friendly view.
func renderPluginStatusMarkdown(s *pluginStatusResponse) string {
	var sb strings.Builder
	sb.WriteString("# Plugin Status\n\n")

	if !s.Enabled {
		sb.WriteString("**Status:** disabled\n\n")
		if s.Reason != "" {
			fmt.Fprintf(&sb, "_%s_\n\n", s.Reason)
		}
		sb.WriteString("To enable: set `nexus.plugins.enabled: true` in `~/.neo/nexus.yaml` and add entries to `~/.neo/plugins.yaml`.\n")
		return sb.String()
	}

	fmt.Fprintf(&sb, "**Status:** enabled — manifest_version=%d\n\n", s.ManifestVersion)

	if len(s.Plugins) == 0 {
		sb.WriteString("_No plugins running._ Either no manifest entries are `enabled: true`, or all spawn attempts failed (see Errors below).\n\n")
	} else {
		sb.WriteString("## Running plugins\n\n")
		sb.WriteString("| Plugin | PID | Status |\n|---|---|---|\n")
		// Stable sort for deterministic output.
		sort.Slice(s.Plugins, func(i, j int) bool { return s.Plugins[i].Name < s.Plugins[j].Name })
		for _, p := range s.Plugins {
			fmt.Fprintf(&sb, "| `%s` | %d | %s |\n", p.Name, p.PID, p.Status)
		}
		sb.WriteString("\n")
	}

	if len(s.Tools) > 0 {
		fmt.Fprintf(&sb, "## Aggregated tools (%d)\n\n", len(s.Tools))
		toolsCopy := append([]string(nil), s.Tools...)
		sort.Strings(toolsCopy)
		for _, name := range toolsCopy {
			fmt.Fprintf(&sb, "- `%s`\n", name)
		}
		sb.WriteString("\n")
	}

	if len(s.Errors) > 0 {
		sb.WriteString("## Errors\n\n")
		// Stable sort by plugin name.
		names := make([]string, 0, len(s.Errors))
		for n := range s.Errors {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Fprintf(&sb, "- **%s**: %s\n", n, s.Errors[n])
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

// renderPluginStatusError formats a fail-soft error response. The radar
// handler returns this instead of an error so a transient Nexus issue
// doesn't fail the whole tool call — the agent sees a useful message.
func renderPluginStatusError(detail string) string {
	return fmt.Sprintf("# Plugin Status\n\n**Status:** unreachable\n\n%s\n\nVerify Nexus is running and `nexus.plugins.enabled` is set.", detail)
}

// fetchPluginsSegment is a best-effort renderer for the BRIEFING compact
// line. Returns empty string on any error, when plugins are disabled, or
// when none are configured — silence is preferable to noise for
// orthogonal subsystems on the always-on output.
//
// Format examples:
//
//	" | plugins: 2 active (jira, github)"
//	" | plugins: 1/2 errored"  (when at least one plugin failed boot)
//
// Leading " | " matches the existing compact-line segment convention.
func fetchPluginsSegment(t *RadarTool) string {
	if t == nil || t.cfg == nil {
		return ""
	}
	url := nexusBaseURL(t) + "/api/v1/plugins"
	client := sre.SafeInternalHTTPClient(2) // tight timeout — BRIEFING is hot path
	resp, err := client.Get(url) //nolint:gosec // G107-WRAPPED-SAFE-CLIENT: SafeInternalHTTPClient allows only loopback; URL built from validated config.
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ""
	}
	var status pluginStatusResponse
	if err := json.Unmarshal(body, &status); err != nil {
		return ""
	}
	if !status.Enabled {
		return ""
	}
	active := len(status.Plugins)
	errored := len(status.Errors)
	if active == 0 && errored == 0 {
		return ""
	}
	if errored > 0 {
		return fmt.Sprintf(" | plugins: %d/%d errored", errored, active+errored)
	}
	return formatPluginNames(status.Plugins, active)
}

// formatPluginNames renders the active-plugin segment with zombie
// markers when applicable. Extracted from fetchPluginsSegment to keep
// per-function CC ≤ 15. [ÉPICA 152.C]
//
// Each plugin name is annotated "⚠️<name>:zombie" when its embedded
// health snapshot indicates the dispatcher is non-functional. The
// compact line then becomes "plugins: N active K⚠️zombie (...)" so the
// operator sees the count in the BRIEFING summary even when names get
// truncated for length.
func formatPluginNames(plugins []pluginRunInfo, active int) string {
	zombieCount := 0
	names := make([]string, 0, active)
	for _, p := range plugins {
		display := p.Name
		if p.Health != nil && p.Health.IsZombie() {
			display = "⚠️" + display + ":zombie"
			zombieCount++
		}
		names = append(names, display)
	}
	sort.Strings(names)
	display := strings.Join(names, ", ")
	if len(display) > 60 {
		display = display[:57] + "..."
	}
	if zombieCount > 0 {
		return fmt.Sprintf(" | plugins: %d active %d⚠️zombie (%s)", active, zombieCount, display)
	}
	return fmt.Sprintf(" | plugins: %d active (%s)", active, display)
}
