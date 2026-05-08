package knowledge_test

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/knowledge"
)

func openTestStore(t *testing.T) *knowledge.KnowledgeStore {
	t.Helper()
	ks, err := knowledge.Open(t.TempDir() + "/knowledge.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { ks.Close() })
	return ks
}

func TestPutGet(t *testing.T) {
	ks := openTestStore(t)
	e := knowledge.KnowledgeEntry{
		Key:       "dto.CreateUserRequest",
		Namespace: knowledge.NSContracts,
		Content:   "requires tenant_id and email",
		Tags:      []string{"auth", "users"},
		Hot:       true,
	}
	if err := ks.Put(knowledge.NSContracts, e.Key, e); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := ks.Get(knowledge.NSContracts, e.Key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Content != e.Content {
		t.Errorf("Content = %q, want %q", got.Content, e.Content)
	}
	if !got.Hot {
		t.Error("Hot should be true")
	}
	if got.CreatedAt == 0 {
		t.Error("CreatedAt should be set")
	}
}

func TestGetNotFound(t *testing.T) {
	ks := openTestStore(t)
	_, err := ks.Get(knowledge.NSContracts, "missing")
	if !errors.Is(err, knowledge.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestDelete(t *testing.T) {
	ks := openTestStore(t)
	e := knowledge.KnowledgeEntry{Key: "x", Namespace: knowledge.NSRules, Content: "y"}
	_ = ks.Put(knowledge.NSRules, "x", e)

	if err := ks.Delete(knowledge.NSRules, "x"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err := ks.Get(knowledge.NSRules, "x")
	if !errors.Is(err, knowledge.ErrNotFound) {
		t.Errorf("expected ErrNotFound after delete, got %v", err)
	}
	// Idempotent delete.
	if err := ks.Delete(knowledge.NSRules, "x"); err != nil {
		t.Errorf("double-delete should not error: %v", err)
	}
}

func TestList(t *testing.T) {
	ks := openTestStore(t)
	for i, key := range []string{"a", "b", "c"} {
		tags := []string{}
		if i%2 == 0 {
			tags = []string{"even"}
		}
		_ = ks.Put(knowledge.NSEnums, key, knowledge.KnowledgeEntry{
			Key: key, Namespace: knowledge.NSEnums, Content: key, Tags: tags,
		})
	}
	all, err := ks.List(knowledge.NSEnums, "")
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("expected 3, got %d", len(all))
	}
	even, err := ks.List(knowledge.NSEnums, "even")
	if err != nil {
		t.Fatalf("List even: %v", err)
	}
	if len(even) != 2 {
		t.Errorf("expected 2 even, got %d", len(even))
	}
}

func TestSearch(t *testing.T) {
	ks := openTestStore(t)
	_ = ks.Put(knowledge.NSContracts, "POST /api/users", knowledge.KnowledgeEntry{
		Key: "POST /api/users", Namespace: knowledge.NSContracts,
		Content: "creates a user with tenant_id",
	})
	_ = ks.Put(knowledge.NSContracts, "GET /api/users", knowledge.KnowledgeEntry{
		Key: "GET /api/users", Namespace: knowledge.NSContracts,
		Content: "lists users for a tenant",
	})
	_ = ks.Put(knowledge.NSContracts, "DELETE /api/sessions", knowledge.KnowledgeEntry{
		Key: "DELETE /api/sessions", Namespace: knowledge.NSContracts,
		Content: "terminates an auth session",
	})

	results, err := ks.Search(knowledge.NSContracts, "tenant", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("expected 2 tenant results, got %d", len(results))
	}
	single, _ := ks.Search(knowledge.NSContracts, "/api/sessions", 10)
	if len(single) != 1 {
		t.Errorf("expected 1 session result, got %d", len(single))
	}
}

func TestListNamespaces(t *testing.T) {
	ks := openTestStore(t)
	_ = ks.Put(knowledge.NSContracts, "a", knowledge.KnowledgeEntry{Key: "a", Namespace: knowledge.NSContracts})
	_ = ks.Put(knowledge.NSTypes, "b", knowledge.KnowledgeEntry{Key: "b", Namespace: knowledge.NSTypes})
	nss, err := ks.ListNamespaces()
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}
	if len(nss) != 2 {
		t.Errorf("expected 2 namespaces, got %d: %v", len(nss), nss)
	}
}

func TestHotCache(t *testing.T) {
	ks := openTestStore(t)
	_ = ks.Put(knowledge.NSContracts, "hot-entry", knowledge.KnowledgeEntry{
		Key: "hot-entry", Namespace: knowledge.NSContracts, Content: "hot content", Hot: true,
	})
	_ = ks.Put(knowledge.NSContracts, "cold-entry", knowledge.KnowledgeEntry{
		Key: "cold-entry", Namespace: knowledge.NSContracts, Content: "cold content", Hot: false,
	})
	hc := knowledge.NewHotCache()
	if err := hc.Load(ks); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if e, ok := hc.Get(knowledge.NSContracts, "hot-entry"); !ok || e.Content != "hot content" {
		t.Errorf("hot entry not in cache: %v, %v", ok, e)
	}
	if _, ok := hc.Get(knowledge.NSContracts, "cold-entry"); ok {
		t.Error("cold entry should not be in hot cache")
	}
	hc.Delete(knowledge.NSContracts, "hot-entry")
	if _, ok := hc.Get(knowledge.NSContracts, "hot-entry"); ok {
		t.Error("entry should be gone after Delete")
	}
	hot, _ := hc.Stats()
	if hot != 0 {
		t.Errorf("hot count should be 0, got %d", hot)
	}
}

func TestDualLayerSync(t *testing.T) {
	dir := t.TempDir()
	syncDir := t.TempDir()
	ks, err := knowledge.Open(dir + "/knowledge.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { ks.Close() })
	ks.SetSyncDir(syncDir)

	e := knowledge.KnowledgeEntry{
		Key: "POST /api/users", Namespace: knowledge.NSContracts,
		Content: "creates a user", Tags: []string{"api"}, Hot: true,
	}
	if err := ks.Put(knowledge.NSContracts, e.Key, e); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// .md file must exist after Put — find it by walking syncDir.
	var mdPath string
	entries, err := os.ReadDir(syncDir + "/" + knowledge.NSContracts)
	if err != nil {
		t.Fatalf("companion .md dir not created: %v", err)
	}
	for _, de := range entries {
		if strings.HasSuffix(de.Name(), ".md") {
			mdPath = syncDir + "/" + knowledge.NSContracts + "/" + de.Name()
			break
		}
	}
	if mdPath == "" {
		t.Fatal("no .md file created in syncDir after Put")
	}
	data, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatalf("read companion .md: %v", err)
	}
	if !strings.Contains(string(data), "creates a user") {
		t.Errorf(".md missing content: %s", data)
	}

	// Delete must remove the .md file.
	if err := ks.Delete(knowledge.NSContracts, e.Key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(mdPath); !os.IsNotExist(err) {
		t.Errorf("companion .md should be removed after Delete")
	}
}
