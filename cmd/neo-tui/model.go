package main

// model.go — bubbletea root Model.
// [PILAR-XXVII/246.C]

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/ensamblatec/neoanvil/cmd/neo-tui/views"
)

var (
	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#00FF00")).
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#00FF00")).
			Padding(0, 1)

	tabBarStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#AAAAAA"))

	tabActiveStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#00FFFF")).
			Underline(true)

	bodyStyle = lipgloss.NewStyle().
			Padding(1, 2)

	footerStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#888888")).
			MarginTop(1)

	errStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FF5555")).
			Bold(true)
)

// tabNames must align with the renderer switch in View().
var tabNames = []string{
	"Overview", "Tools", "Tokens", "Mutations",
	"Memory", "System", "Rules", "Incidents",
}

const refreshInterval = 3 * time.Second

// tickMsg triggers a metrics refresh.
type tickMsg time.Time

// metricsMsg carries the newest Snapshot or the error from FetchMetrics.
type metricsMsg struct {
	snap *Snapshot
	err  error
}

// model is the root bubbletea Model.
type model struct {
	client    *Client
	wsID      string
	wsLabel   string
	tab       int
	snap      *Snapshot
	err       error
	status    string // "connecting" | "ok" | "stale" | "down"
	lastOK    time.Time
	width     int
	height    int
}

// newModel builds the root model. wsID may be the raw workspace id or
// name; Nexus proxies both.
func newModel(client *Client, wsID, wsLabel string) model {
	return model{
		client:  client,
		wsID:    wsID,
		wsLabel: wsLabel,
		status:  "connecting",
	}
}

// Init kicks off the first refresh + the ticker.
func (m model) Init() tea.Cmd {
	return tea.Batch(m.fetchCmd(), m.tickCmd())
}

func (m model) fetchCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		snap, err := m.client.FetchMetrics(ctx, m.wsID)
		return metricsMsg{snap: snap, err: err}
	}
}

func (m model) tickCmd() tea.Cmd {
	return tea.Tick(refreshInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// Update handles keyboard + tick + metrics events.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(msg)
	case tickMsg:
		return m, tea.Batch(m.fetchCmd(), m.tickCmd())
	case metricsMsg:
		if msg.err != nil {
			m.err = msg.err
			if time.Since(m.lastOK) > 10*time.Second {
				m.status = "down"
			} else {
				m.status = "stale"
			}
			return m, nil
		}
		m.snap = msg.snap
		m.err = nil
		m.status = "ok"
		m.lastOK = time.Now()
		return m, nil
	}
	return m, nil
}

func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "r":
		return m, m.fetchCmd()
	case "tab", "right", "l":
		m.tab = (m.tab + 1) % len(tabNames)
		return m, nil
	case "shift+tab", "left", "h":
		m.tab = (m.tab - 1 + len(tabNames)) % len(tabNames)
		return m, nil
	case "1", "2", "3", "4", "5", "6", "7", "8":
		idx := int(msg.String()[0]-'0') - 1
		if idx >= 0 && idx < len(tabNames) {
			m.tab = idx
		}
		return m, nil
	}
	return m, nil
}

// View renders the full screen.
func (m model) View() string {
	var b strings.Builder
	b.WriteString(m.renderHeader())
	b.WriteString("\n")
	b.WriteString(m.renderTabBar())
	b.WriteString("\n")
	b.WriteString(m.renderBody())
	b.WriteString("\n")
	b.WriteString(m.renderFooter())
	return b.String()
}

func (m model) renderHeader() string {
	title := fmt.Sprintf("neo-tui  %s · %s", statusDot(m.status), m.wsLabel)
	return headerStyle.Render(title)
}

func (m model) renderTabBar() string {
	cells := make([]string, len(tabNames))
	for i, name := range tabNames {
		label := fmt.Sprintf("%d.%s", i+1, name)
		if i == m.tab {
			cells[i] = tabActiveStyle.Render(label)
		} else {
			cells[i] = tabBarStyle.Render(label)
		}
	}
	return strings.Join(cells, "  ")
}

func (m model) renderBody() string {
	if m.err != nil && m.snap == nil {
		return bodyStyle.Render(errStyle.Render("Cannot reach Nexus: ") + m.err.Error() +
			"\n\nIs neo-nexus running on " + m.client.NexusBase + "? Check with `ps aux | grep neo-nexus`.")
	}
	if m.snap == nil {
		return bodyStyle.Render("Loading metrics from " + m.client.NexusBase + " …")
	}
	vs := adaptSnapshot(m.snap)
	var out string
	switch m.tab {
	case 0:
		out = views.RenderOverview(vs)
	case 1:
		out = views.RenderTools(vs)
	case 2:
		out = views.RenderTokens(vs)
	case 3:
		out = views.RenderMutations(vs)
	case 4:
		out = views.RenderMemory(vs)
	case 5:
		out = views.RenderSystem(vs)
	case 6:
		out = views.RenderRules(vs)
	case 7:
		out = views.RenderIncidents(vs)
	default:
		out = "(unknown tab)"
	}
	return bodyStyle.Render(out)
}

func (m model) renderFooter() string {
	hints := "[1-8] tabs · [tab] next · [r] refresh · [q] quit"
	return footerStyle.Render(hints + "  |  " + m.client.NexusBase)
}

// statusDot returns a coloured unicode dot for the status indicator.
func statusDot(s string) string {
	switch s {
	case "ok":
		return "\033[32m●\033[0m connected"
	case "stale":
		return "\033[33m●\033[0m stale"
	case "down":
		return "\033[31m●\033[0m down"
	default:
		return "\033[90m●\033[0m " + s
	}
}

// adaptSnapshot flattens the JSON payload into the views-package
// snapshot, keeping views free of the api types.
func adaptSnapshot(s *Snapshot) views.Snapshot {
	v := views.Snapshot{
		WorkspaceID:   s.WorkspaceID,
		WorkspaceName: s.WorkspaceName,
		UptimeSeconds: s.UptimeSeconds,
		GeneratedAt:   s.GeneratedAt,

		HeapMB:         s.Memory.HeapMB,
		StackMB:        s.Memory.StackMB,
		Goroutines:     s.Memory.Goroutines,
		CPGHeapMB:      s.Memory.CPGHeapMB,
		CPGHeapLimitMB: s.Memory.CPGHeapLimitMB,
		CPGHeapPct:     s.Memory.CPGHeapPct,
		QueryHit:       s.Memory.QueryCacheHit,
		TextHit:        s.Memory.TextCacheHit,
		EmbHit:         s.Memory.EmbCacheHit,

		Total24h: s.Tools.Total24h,

		TokensMCPIn:       s.Tokens.MCPTraffic.InputTokens,
		TokensMCPOut:      s.Tokens.MCPTraffic.OutputTokens,
		TokensInternalIn:  s.Tokens.InternalInference.InputTokens,
		TokensInternalOut: s.Tokens.InternalInference.OutputTokens,
		TokensCostUSD:     s.Tokens.TodayCostUSD,
		ByAgent:           mergeCountMaps(s.Tokens.MCPTraffic.ByAgent, s.Tokens.InternalInference.ByAgent),
		ByTool:            mergeCountMaps(s.Tokens.MCPTraffic.ByTool, s.Tokens.InternalInference.ByTool),

		Certified24h: s.Mutations.Certified24h,
		Bypassed24h:  s.Mutations.Bypassed24h,
	}
	v.TopByCalls = adaptTools(s.Tools.TopByCalls)
	v.TopByErrors = adaptTools(s.Tools.TopByErrors)
	v.TopByP99 = adaptTools(s.Tools.TopByP99)

	for _, h := range s.Mutations.TopHotspots {
		v.Hotspots = append(v.Hotspots, views.HotspotRow{Path: h.Path, Count: h.Count})
	}
	for _, d := range s.Tokens.Last7Days {
		v.Last7Days = append(v.Last7Days, views.DayRow{
			Day: d.Day, MCPInput: d.MCPInput, MCPOutput: d.MCPOutput,
			InternalInput: d.InternalInput, InternalOutput: d.InternalOutput,
			CostUSD: d.CostUSD,
		})
	}
	for _, e := range s.Events {
		v.Events = append(v.Events, views.EventRow{
			Timestamp: e.Timestamp, Type: e.Type, Severity: e.Severity,
		})
	}
	return v
}

func adaptTools(in []ToolStats) []views.ToolRow {
	out := make([]views.ToolRow, 0, len(in))
	for _, t := range in {
		out = append(out, views.ToolRow{
			Name: t.Name, Calls: t.Calls, Errors: t.Errors,
			ErrorRate: t.ErrorRate, P99Ms: t.P99Ms,
		})
	}
	return out
}

func mergeCountMaps(a, b map[string]int) map[string]int {
	out := make(map[string]int, len(a)+len(b))
	for k, v := range a {
		out[k] += v
	}
	for k, v := range b {
		out[k] += v
	}
	return out
}
