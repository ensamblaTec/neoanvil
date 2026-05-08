package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeNexusForPostCommit returns an httptest server that mimics Nexus's
// /health, /api/v1/plugins, and /mcp/message endpoints. validTickets controls
// which jira/get_context lookups return a valid ticket vs not-found.
// prepareDocPackBehavior controls whether prepare_doc_pack returns success
// or a "Files list is required" error (mimicking real plugin behavior).
func fakeNexusForPostCommit(t *testing.T, validTickets map[string]bool) (*httptest.Server, *int) {
	t.Helper()
	docPackCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/api/v1/plugins":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"name":"jira","status":"running"}]`))
		case "/mcp/message":
			bodyBytes, _ := io.ReadAll(r.Body)
			payload := string(bodyBytes)
			w.Header().Set("Content-Type", "application/json")

			// Identify which ticket is being requested by scanning payload.
			var ticket string
			for k := range validTickets {
				if strings.Contains(payload, `"ticket_id":"`+k+`"`) {
					ticket = k
					break
				}
			}
			if ticket == "" {
				// Look for any ticket pattern even if not pre-seeded.
				for _, candidate := range []string{"MCPI-9999", "ADR-009", "Z0-9", "PHANTOM-1"} {
					if strings.Contains(payload, `"ticket_id":"`+candidate+`"`) {
						ticket = candidate
						break
					}
				}
			}

			isValid := validTickets[ticket]
			// Match action loosely — the hook formats JSON-RPC with varying
			// whitespace ("action":"X" vs "action": "X" depending on the
			// caller). Substring match on the action verb is enough.
			switch {
			case strings.Contains(payload, "get_context"):
				if isValid {
					_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"ok"}]}}`))
				} else {
					_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"issue not found (404)"}}`))
				}
			case strings.Contains(payload, "prepare_doc_pack"):
				docPackCalls++
				if isValid {
					_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"Attached as zip"}]}}`))
				} else {
					_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"Files list is required"}}`))
				}
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &docPackCalls
}

// runPostCommitInRepo creates a temp git repo, commits a file with the given
// message, and runs the post-commit hook against the configured Nexus stub.
// Returns combined stdout+stderr of the hook invocation and the doc-pack call count.
func runPostCommitInRepo(t *testing.T, commitMsg, nexusURL string) string {
	t.Helper()
	dir := t.TempDir()

	// Init repo.
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
	} {
		//nolint:gosec // G204-LITERAL-BIN: fixed git, test-only.
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// Make the hook lib + scripts available inside the temp repo so post-commit
	// can source them via $REPO_ROOT/scripts/git-hooks/.
	hookDir := filepath.Join(dir, "scripts", "git-hooks")
	if err := os.MkdirAll(hookDir, 0755); err != nil {
		t.Fatal(err)
	}
	repoRoot := repoRootForTest(t)
	for _, name := range []string{"post-commit", "lib-jira-tickets.sh", "sync-master-plan.sh"} {
		src := filepath.Join(repoRoot, "scripts", "git-hooks", name)
		dst := filepath.Join(hookDir, name)
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, data, 0755); err != nil {
			t.Fatal(err)
		}
	}

	// Seed a file + commit it with the controlled message.
	fpath := filepath.Join(dir, "x.txt")
	if err := os.WriteFile(fpath, []byte("hi"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "x.txt"},
		{"commit", "--no-gpg-sign", "-m", commitMsg},
	} {
		//nolint:gosec // G204-LITERAL-BIN: fixed git, test-only.
		_, _ = exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	}

	// Now invoke the hook directly (we don't install it — git's commit already
	// happened above; we just exercise the hook script here in isolation).
	hookPath := filepath.Join(hookDir, "post-commit")
	//nolint:gosec // G204-LITERAL-BIN: fixed bash, test-only.
	cmd := exec.Command("bash", hookPath)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"NEO_NEXUS_URL="+nexusURL,
		"NEO_REPO_ROOT="+dir,
		"NEO_HOOK_DISABLE=0",
		"NEO_HOOK_QUIET=1", // suppress success messages — we only assert errors/warnings
		"NEO_WORKSPACE_ID=test-ws-1",
	)
	out, _ := cmd.CombinedOutput()
	return string(out)
}

// TestPostCommit_SubjectClean_NoTickets verifies that a clean commit message
// (no ticket-shaped strings anywhere) results in zero doc-pack calls. [139.4a]
func TestPostCommit_SubjectClean_NoTickets(t *testing.T) {
	srv, calls := fakeNexusForPostCommit(t, map[string]bool{})
	out := runPostCommitInRepo(t, "chore: trivial cleanup", srv.URL)
	if *calls != 0 {
		t.Errorf("expected 0 doc-pack calls, got %d", *calls)
	}
	if strings.Contains(out, "ERROR") {
		t.Errorf("expected clean output, got error: %q", out)
	}
}

// TestPostCommit_SubjectValid_OneFire verifies a valid ticket in subject
// triggers exactly one prepare_doc_pack call. [139.4b]
func TestPostCommit_SubjectValid_OneFire(t *testing.T) {
	srv, calls := fakeNexusForPostCommit(t, map[string]bool{"MCPI-52": true})
	out := runPostCommitInRepo(t,
		"feat(jira): MCPI-52 closes the thing",
		srv.URL,
	)
	if *calls != 1 {
		t.Errorf("expected exactly 1 doc-pack call, got %d\noutput=%s", *calls, out)
	}
}

// TestPostCommit_BodyRefs_Ignored verifies that ticket-shaped strings in the
// commit body do NOT trigger doc-pack calls. The exact bug Épica 139 fixes:
// body containing "ADR-009", "Z0-9", or "MCPI-130" must be ignored. [139.4c]
func TestPostCommit_BodyRefs_Ignored(t *testing.T) {
	srv, calls := fakeNexusForPostCommit(t, map[string]bool{})
	commitMsg := `chore: clean refactor

Body mentions ADR-009 and Z0-9 in regex examples.
Also MCPI-130 (typo from old commit). None should fire.`
	out := runPostCommitInRepo(t, commitMsg, srv.URL)
	if *calls != 0 {
		t.Errorf("expected 0 doc-pack calls (body refs must be ignored), got %d\noutput=%s",
			*calls, out)
	}
	if strings.Contains(out, "ERROR") {
		t.Errorf("expected zero ERRORs (no fires = no failures), got: %q", out)
	}
}

// TestPostCommit_PhantomSubjectTicket_SkippedSilently verifies that a phantom
// ticket in the subject (e.g. operator typo MCPI-9999 that doesn't exist in
// Jira) is validated pre-fire and skipped — does NOT call prepare_doc_pack.
// [139.4d]
func TestPostCommit_PhantomSubjectTicket_SkippedSilently(t *testing.T) {
	// MCPI-9999 is NOT in valid set — Nexus will return not-found on get_context.
	srv, calls := fakeNexusForPostCommit(t, map[string]bool{})
	out := runPostCommitInRepo(t,
		"fix(jira): MCPI-9999 phantom typo",
		srv.URL,
	)
	if *calls != 0 {
		t.Errorf("expected 0 doc-pack calls (phantom must be validated away), got %d", *calls)
	}
	if !strings.Contains(out, "skip MCPI-9999: not in Jira") {
		t.Errorf("expected skip-warning in stderr, got %q", out)
	}
}

// TestPostCommit_ValidPlusPhantom_OnlyValidFires verifies that mixed input
// (one real ticket + one phantom in subject) only fires for the real one.
// Real-world: "feat: MCPI-52 + MCPI-9999 paired fix" → 1 fire, 1 skip.
func TestPostCommit_ValidPlusPhantom_OnlyValidFires(t *testing.T) {
	srv, calls := fakeNexusForPostCommit(t, map[string]bool{"MCPI-52": true})
	out := runPostCommitInRepo(t,
		"feat(jira): MCPI-52 fixes thing also references MCPI-9999",
		srv.URL,
	)
	if *calls != 1 {
		t.Errorf("expected exactly 1 doc-pack call (only MCPI-52 valid), got %d", *calls)
	}
	if !strings.Contains(out, "skip MCPI-9999") {
		t.Errorf("expected MCPI-9999 skip warning, got %q", out)
	}
}
