package main

// radar_inbox.go — INBOX intent handler. PILAR LX / Épica 331.C.
//
// Exposes `neo_radar(intent:"INBOX")` as the agent-facing interface to the
// inbox namespace. Supports two modes:
//
//   1. List mode   (no key)       → returns table of unread / all / urgent entries
//                                    targeting the current workspace. Default
//                                    filter = "unread".
//   2. Fetch mode  (key given)    → returns the full entry body and marks it
//                                    read (ReadAt = now()). Subsequent BRIEFING
//                                    won't count it as unread.
//
// Args:
//   filter    = "unread" | "all" | "urgent"  (default "unread")
//   key       = "to-<wsID>-<topic>"          (optional — fetch mode)

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/knowledge"
)

// handleInbox routes INBOX invocations to list or fetch mode. [331.C]
func (t *RadarTool) handleInbox(_ context.Context, args map[string]any) (any, error) {
	if t.knowledgeStore == nil {
		return nil, fmt.Errorf("INBOX: project federation not active (KnowledgeStore not wired)")
	}
	if keyArg, _ := args["key"].(string); keyArg != "" {
		return t.inboxFetch(keyArg)
	}
	filter, _ := args["filter"].(string)
	if filter == "" {
		filter = "unread"
	}
	return t.inboxList(filter)
}

// inboxFetch returns the full entry + marks it read. [331.C]
func (t *RadarTool) inboxFetch(key string) (any, error) {
	if err := knowledge.ValidateInboxKey(key); err != nil {
		return nil, fmt.Errorf("INBOX fetch: %w", err)
	}
	entry, err := t.knowledgeStore.Get(knowledge.NSInbox, key)
	if err != nil {
		return nil, fmt.Errorf("INBOX fetch: %w", err)
	}
	// Mark read (idempotent — updates timestamp).
	if mErr := t.knowledgeStore.MarkInboxRead(key); mErr != nil {
		// Non-fatal — log and still return the body.
		return mcpText(fmt.Sprintf(
			"⚠️ marked-read failed: %v\n\n%s", mErr, renderInboxEntry(entry),
		)), nil
	}
	return mcpText(renderInboxEntry(entry)), nil
}

// inboxList returns the filtered table of inbox entries for the current ws. [331.C]
func (t *RadarTool) inboxList(filter string) (any, error) {
	wsID := resolveWorkspaceID(t.workspace)
	if wsID == "" {
		return nil, fmt.Errorf("INBOX: cannot resolve current workspace ID (is this workspace registered in ~/.neo/workspaces.json?)")
	}
	unreadOnly := filter == "unread" || filter == "urgent"
	entries, err := t.knowledgeStore.ListInboxFor(wsID, unreadOnly)
	if err != nil {
		return nil, fmt.Errorf("INBOX list: %w", err)
	}
	if filter == "urgent" {
		entries = filterInboxUrgent(entries)
	}
	return mcpText(renderInboxTable(wsID, filter, entries)), nil
}

// filterInboxUrgent keeps only entries with Priority == "urgent". [331.C]
func filterInboxUrgent(in []knowledge.KnowledgeEntry) []knowledge.KnowledgeEntry {
	out := in[:0]
	for _, e := range in {
		if e.Priority == knowledge.InboxPriorityUrgent {
			out = append(out, e)
		}
	}
	return out
}

// renderInboxTable formats a list response as Markdown. [331.C]
func renderInboxTable(wsID, filter string, entries []knowledge.KnowledgeEntry) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "## 📬 Inbox — %s (filter: %s)\n\n", wsID, filter)
	if len(entries) == 0 {
		sb.WriteString("_No entries._\n")
		return sb.String()
	}
	sb.WriteString("| Key | From | Priority | Age | Preview |\n")
	sb.WriteString("|-----|------|----------|-----|--------|\n")
	now := time.Now().Unix()
	for _, e := range entries {
		age := formatInboxAge(now - e.UpdatedAt)
		readMark := ""
		if e.ReadAt != 0 {
			readMark = " ✓"
		}
		priority := e.Priority
		if priority == "" {
			priority = knowledge.InboxPriorityNormal
		}
		if priority == knowledge.InboxPriorityUrgent {
			priority = "🔴 " + priority
		}
		fmt.Fprintf(&sb, "| `%s`%s | %s | %s | %s | %s |\n",
			e.Key, readMark, e.From, priority, age, inboxPreview(e.Content, 60))
	}
	sb.WriteString("\nFetch with: `neo_radar(intent:\"INBOX\", key:\"<key>\")` — marks as read.\n")
	return sb.String()
}

// renderInboxEntry formats a single entry body for fetch mode. [331.C]
func renderInboxEntry(e *knowledge.KnowledgeEntry) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "## 📬 %s\n\n", e.Key)
	fmt.Fprintf(&sb, "- **From:** %s\n", e.From)
	fmt.Fprintf(&sb, "- **Priority:** %s\n", priorityOrDefault(e.Priority))
	fmt.Fprintf(&sb, "- **Sent:** %s\n", time.Unix(e.UpdatedAt, 0).UTC().Format(time.RFC3339))
	if e.ThreadID != "" {
		fmt.Fprintf(&sb, "- **Thread:** %s\n", e.ThreadID)
	}
	sb.WriteString("\n---\n\n")
	sb.WriteString(e.Content)
	sb.WriteString("\n")
	return sb.String()
}

// inboxPreview returns a single-line snippet for the list table. [331.C]
func inboxPreview(body string, max int) string {
	snippet := strings.ReplaceAll(body, "\n", " ")
	snippet = strings.ReplaceAll(snippet, "|", "\\|")
	if len(snippet) > max {
		snippet = snippet[:max] + "…"
	}
	return snippet
}

// formatInboxAge prints deltaSeconds as "Nm" / "Nh" / "Nd". [331.C]
func formatInboxAge(deltaSeconds int64) string {
	if deltaSeconds < 60 {
		return fmt.Sprintf("%ds", deltaSeconds)
	}
	if deltaSeconds < 3600 {
		return fmt.Sprintf("%dm", deltaSeconds/60)
	}
	if deltaSeconds < 86400 {
		return fmt.Sprintf("%dh", deltaSeconds/3600)
	}
	return fmt.Sprintf("%dd", deltaSeconds/86400)
}

func priorityOrDefault(p string) string {
	if p == "" {
		return knowledge.InboxPriorityNormal
	}
	return p
}
