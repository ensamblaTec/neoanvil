package federation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindNeoProjectDir_Found(t *testing.T) {
	root := t.TempDir()
	projDir := filepath.Join(root, ".neo-project")
	if err := os.MkdirAll(projDir, 0o750); err != nil {
		t.Fatal(err)
	}

	got, ok := FindNeoProjectDir(root)
	if !ok {
		t.Fatal("expected to find .neo-project dir")
	}
	if got != projDir {
		t.Errorf("expected %q, got %q", projDir, got)
	}
}

func TestFindNeoProjectDir_NotFound(t *testing.T) {
	dir := t.TempDir()
	_, ok := FindNeoProjectDir(dir)
	if ok {
		t.Error("should not find .neo-project in empty temp dir")
	}
}

func TestFindNeoProjectDir_WalksUp(t *testing.T) {
	root := t.TempDir()
	projDir := filepath.Join(root, ".neo-project")
	if err := os.MkdirAll(projDir, 0o750); err != nil {
		t.Fatal(err)
	}
	subDir := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(subDir, 0o750); err != nil {
		t.Fatal(err)
	}

	got, ok := FindNeoProjectDir(subDir)
	if !ok {
		t.Fatal("expected to find .neo-project by walking up")
	}
	if got != projDir {
		t.Errorf("expected %q, got %q", projDir, got)
	}
}

func TestParseSharedDebt_NoFile(t *testing.T) {
	dir := t.TempDir()
	contracts, err := ParseSharedDebt(dir)
	if err != nil {
		t.Fatalf("ParseSharedDebt on missing file: %v", err)
	}
	if contracts != nil {
		t.Errorf("expected nil for missing file, got %v", contracts)
	}
}

func TestAppendMissingContract_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	if err := AppendMissingContract(dir, "/api/foo", "callerA", "ws-1"); err != nil {
		t.Fatalf("AppendMissingContract: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, sharedDebtFile))
	if err != nil {
		t.Fatalf("file should exist: %v", err)
	}
	if !strings.Contains(string(data), "/api/foo") {
		t.Errorf("file should contain the endpoint: %q", string(data))
	}
}

func TestAppendMissingContract_Appends(t *testing.T) {
	dir := t.TempDir()
	_ = AppendMissingContract(dir, "/api/one", "caller1", "ws-1")
	_ = AppendMissingContract(dir, "/api/two", "caller2", "ws-2")

	contracts, err := ParseSharedDebt(dir)
	if err != nil {
		t.Fatalf("ParseSharedDebt: %v", err)
	}
	if len(contracts) != 2 {
		t.Errorf("expected 2 contracts, got %d", len(contracts))
	}
}

func TestParseSharedDebt_Fields(t *testing.T) {
	dir := t.TempDir()
	_ = AppendMissingContract(dir, "/api/bar", "svc-x", "ws-prod")

	contracts, err := ParseSharedDebt(dir)
	if err != nil {
		t.Fatalf("ParseSharedDebt: %v", err)
	}
	if len(contracts) != 1 {
		t.Fatalf("expected 1 contract, got %d", len(contracts))
	}
	c := contracts[0]
	if c.Endpoint != "/api/bar" {
		t.Errorf("Endpoint: %q", c.Endpoint)
	}
	if c.Caller != "svc-x" {
		t.Errorf("Caller: %q", c.Caller)
	}
	if c.Workspace != "ws-prod" {
		t.Errorf("Workspace: %q", c.Workspace)
	}
	if c.Status == "" {
		t.Error("Status should not be empty")
	}
}

// TestAppendMissingContract_NewFileRowBeforeFooter — regression for 330.H bug:
// when SHARED_DEBT.md is freshly created by AppendMissingContract, the row must
// appear BEFORE the "_Última actualización:" footer, not after.
func TestAppendMissingContract_NewFileRowBeforeFooter(t *testing.T) {
	dir := t.TempDir()
	if err := AppendMissingContract(dir, "/api/v1/foo", "POST", "strategos-32492"); err != nil {
		t.Fatalf("append: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, sharedDebtFile))
	content := string(data)
	rowIdx := strings.Index(content, "/api/v1/foo")
	footerIdx := strings.Index(content, footerPrefix)
	if footerIdx == -1 {
		t.Fatalf("footer missing in new file")
	}
	if rowIdx == -1 {
		t.Fatalf("row not written")
	}
	if rowIdx > footerIdx {
		t.Errorf("row at %d must precede footer at %d", rowIdx, footerIdx)
	}
}

// TestAppendMissingContract_ExistingFileWithFooter — regression for 330.H:
// a hand-maintained SHARED_DEBT.md ending in "_Última actualización:" must get
// new rows inserted BEFORE that footer, and the footer date updated.
func TestAppendMissingContract_ExistingFileWithFooter(t *testing.T) {
	dir := t.TempDir()
	initial := "# Shared Technical Debt\n\n" +
		"## P2 — Medio\n" +
		"_Sin entradas._\n\n---\n\n" +
		"## Contratos de frontera bajo revisión\n\n" +
		"| Endpoint | Requested by | Workspace | Date | Status |\n" +
		"|----------|-------------|-----------|------|--------|\n" +
		"| /api/v1/old | GET | strategos-32492 | 2026-04-20 | ⏳ pending |\n\n" +
		"---\n\n_Última actualización: 2026-04-20_\n"
	path := filepath.Join(dir, sharedDebtFile)
	if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := AppendMissingContract(dir, "/api/v1/new", "POST", "strategos-32492"); err != nil {
		t.Fatalf("append: %v", err)
	}
	data, _ := os.ReadFile(path)
	content := string(data)
	lines := strings.Split(content, "\n")

	// Last non-empty line must be the footer.
	var last string
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			last = strings.TrimSpace(lines[i])
			break
		}
	}
	if !strings.HasPrefix(last, footerPrefix) {
		t.Errorf("last non-empty line must be footer, got %q", last)
	}
	if !strings.Contains(content, "/api/v1/old") {
		t.Errorf("preserved row missing")
	}
	if !strings.Contains(content, "/api/v1/new") {
		t.Errorf("new row not inserted")
	}
	newIdx := strings.Index(content, "/api/v1/new")
	footerIdx := strings.Index(content, footerPrefix)
	if newIdx > footerIdx {
		t.Errorf("new row at %d must precede footer at %d", newIdx, footerIdx)
	}
}

// TestAppendMissingContract_SectionMissing — regression for 330.H: if the
// debt file exists but doesn't yet have the "Contratos de frontera" section,
// the function must synthesize it before the footer.
func TestAppendMissingContract_SectionMissing(t *testing.T) {
	dir := t.TempDir()
	initial := "# Shared Technical Debt\n\n## Random other section\n\n_Sin entradas._\n\n" +
		"---\n\n_Última actualización: 2026-04-20_\n"
	path := filepath.Join(dir, sharedDebtFile)
	if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := AppendMissingContract(dir, "/api/v1/abc", "GET", "x"); err != nil {
		t.Fatalf("append: %v", err)
	}
	data, _ := os.ReadFile(path)
	content := string(data)
	if !strings.Contains(content, missingContractsSection) {
		t.Errorf("section should be synthesized")
	}
	if !strings.Contains(content, "/api/v1/abc") {
		t.Errorf("row not inserted")
	}
	lines := strings.Split(content, "\n")
	var last string
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			last = strings.TrimSpace(lines[i])
			break
		}
	}
	if !strings.HasPrefix(last, footerPrefix) {
		t.Errorf("footer must remain last, got %q", last)
	}
}
