package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeNexusForCommitMsg returns an httptest server that mimics Nexus's
// /health and /mcp/message endpoints. ticketKnown controls whether
// jira/get_context returns a valid issue or a not-found error.
func fakeNexusForCommitMsg(t *testing.T, ticketKnown string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/mcp/message":
			body := make([]byte, 4096)
			n, _ := r.Body.Read(body)
			payload := string(body[:n])
			if !strings.Contains(payload, ticketKnown) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"issue not found (404)"}}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"ok"}]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runCommitMsgHook invokes the hook with the given message and Nexus URL,
// returning combined stdout+stderr.
func runCommitMsgHook(t *testing.T, msg, nexusURL string) string {
	t.Helper()
	dir := t.TempDir()
	msgFile := filepath.Join(dir, "COMMIT_EDITMSG")
	if err := os.WriteFile(msgFile, []byte(msg), 0600); err != nil {
		t.Fatal(err)
	}
	script := repoRootForTest(t) + "/scripts/git-hooks/commit-msg"
	//nolint:gosec // G204-LITERAL-BIN: bash binary literal, args validated test-only.
	cmd := exec.Command("bash", script, msgFile)
	cmd.Env = append(os.Environ(),
		"NEO_NEXUS_URL="+nexusURL,
		"NEO_HOOK_DISABLE=0",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hook exited non-zero (must always be 0 — soft warn): %v\nout=%s", err, out)
	}
	return string(out)
}

// TestCommitMsgWarnsOnPhantomTicket verifies the hook surfaces a stderr
// warning when [EPIC-FINAL <KEY>] references a Jira ticket that doesn't
// exist. The exact 30220ae bug. [134.B.3]
func TestCommitMsgWarnsOnPhantomTicket(t *testing.T) {
	srv := fakeNexusForCommitMsg(t, "MCPI-52") // only MCPI-52 exists
	out := runCommitMsgHook(t,
		"feat(jira): does the thing\n\n[EPIC-FINAL MCPI-130]\n",
		srv.URL,
	)
	if !strings.Contains(out, "WARN: MCPI-130") {
		t.Errorf("expected stderr warning about MCPI-130, got %q", out)
	}
}

// TestCommitMsgSilentOnValidTicket verifies no noise when the EPIC-FINAL
// ticket actually exists.
func TestCommitMsgSilentOnValidTicket(t *testing.T) {
	srv := fakeNexusForCommitMsg(t, "MCPI-52")
	out := runCommitMsgHook(t,
		"feat(sre): finalises something\n\n[EPIC-FINAL MCPI-52]\n",
		srv.URL,
	)
	if strings.Contains(out, "WARN") {
		t.Errorf("expected silent run on valid ticket, got %q", out)
	}
}

// TestCommitMsgSilentWithoutEpicFinal verifies plain commits (no marker)
// don't trigger any HTTP traffic.
func TestCommitMsgSilentWithoutEpicFinal(t *testing.T) {
	srv := fakeNexusForCommitMsg(t, "MCPI-52")
	out := runCommitMsgHook(t,
		"fix(sre): straightforward bug fix",
		srv.URL,
	)
	if out != "" {
		t.Errorf("expected silent run with no EPIC-FINAL marker, got %q", out)
	}
}

// TestCommitMsgFailOpenWhenNexusDown verifies the hook stays silent when
// Nexus is unreachable — soft warning is best-effort, never blocking.
func TestCommitMsgFailOpenWhenNexusDown(t *testing.T) {
	out := runCommitMsgHook(t,
		"feat(jira): closes thing [EPIC-FINAL MCPI-999]",
		"http://127.0.0.1:1", // unused port, connection refused
	)
	if strings.Contains(out, "WARN") {
		t.Errorf("expected silent fail-open when Nexus down, got %q", out)
	}
}
