package auth

import (
	"path/filepath"
	"testing"
)

func TestContextStore_LoadMissingReturnsEmpty(t *testing.T) {
	store, err := LoadContexts(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("LoadContexts: %v", err)
	}
	if len(store.Contexts) != 0 {
		t.Errorf("len(Contexts)=%d want 0", len(store.Contexts))
	}
	if store.Active == nil {
		t.Error("Active map should be non-nil")
	}
}

func TestContextStore_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ctx.json")
	store := &ContextStore{Version: 1, Active: map[string]string{}}

	store.Set(Space{Provider: "jira", SpaceID: "ENG", SpaceName: "Engineering", BoardID: "15", BoardName: "Sprint"})
	store.Set(Space{Provider: "github", SpaceID: "acme/api", SpaceName: "API repo"})
	if err := store.Use("jira", "ENG"); err != nil {
		t.Fatalf("Use: %v", err)
	}

	if err := SaveContexts(store, path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := LoadContexts(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(loaded.Contexts) != 2 {
		t.Errorf("len(Contexts)=%d want 2", len(loaded.Contexts))
	}
	if active := loaded.ActiveSpace("jira"); active == nil || active.SpaceID != "ENG" {
		t.Errorf("Active(jira)=%+v want SpaceID=ENG", active)
	}
	if active := loaded.ActiveSpace("jira"); active.BoardID != "15" || active.BoardName != "Sprint" {
		t.Errorf("Board fields lost: %+v", active)
	}
}

func TestContextStore_SetUpsertsByID(t *testing.T) {
	s := &ContextStore{Version: 1}
	s.Set(Space{Provider: "jira", SpaceID: "ENG", SpaceName: "v1"})
	s.Set(Space{Provider: "jira", SpaceID: "ENG", SpaceName: "v2"})
	if len(s.Contexts) != 1 {
		t.Errorf("len(Contexts)=%d want 1 after upsert", len(s.Contexts))
	}
	if s.Contexts[0].SpaceName != "v2" {
		t.Errorf("SpaceName=%q want v2", s.Contexts[0].SpaceName)
	}
}

func TestContextStore_SetStampsUpdatedAt(t *testing.T) {
	s := &ContextStore{Version: 1}
	s.Set(Space{Provider: "jira", SpaceID: "ENG"})
	if s.Contexts[0].UpdatedAt == "" {
		t.Error("UpdatedAt should be auto-stamped")
	}
}

func TestContextStore_UseRequiresRegisteredSpace(t *testing.T) {
	s := &ContextStore{Version: 1}
	if err := s.Use("jira", "ENG"); err == nil {
		t.Error("Use on unregistered space should fail")
	}
}

func TestContextStore_UseEmptyArgsFails(t *testing.T) {
	s := &ContextStore{Version: 1}
	s.Set(Space{Provider: "jira", SpaceID: "ENG"})
	if err := s.Use("", "ENG"); err == nil {
		t.Error("empty provider should fail")
	}
	if err := s.Use("jira", ""); err == nil {
		t.Error("empty space_id should fail")
	}
}

func TestContextStore_ActiveReturnsNilWhenUnset(t *testing.T) {
	s := &ContextStore{Version: 1}
	if s.ActiveSpace("jira") != nil {
		t.Error("Active should return nil when no provider has been activated")
	}
}

func TestContextStore_ActiveStaleAfterRemove(t *testing.T) {
	s := &ContextStore{Version: 1, Active: map[string]string{}}
	s.Set(Space{Provider: "jira", SpaceID: "ENG"})
	_ = s.Use("jira", "ENG")
	s.Remove("jira", "ENG")
	if s.ActiveSpace("jira") != nil {
		t.Error("Active should be cleared after Remove")
	}
}

func TestContextStore_ListByProvider(t *testing.T) {
	s := &ContextStore{Version: 1}
	s.Set(Space{Provider: "jira", SpaceID: "ENG"})
	s.Set(Space{Provider: "jira", SpaceID: "OPS"})
	s.Set(Space{Provider: "github", SpaceID: "acme/api"})

	jiraSpaces := s.ListByProvider("jira")
	if len(jiraSpaces) != 2 {
		t.Errorf("jira count=%d want 2", len(jiraSpaces))
	}
	ghSpaces := s.ListByProvider("github")
	if len(ghSpaces) != 1 {
		t.Errorf("github count=%d want 1", len(ghSpaces))
	}
	if got := s.ListByProvider("missing"); len(got) != 0 {
		t.Errorf("unknown provider count=%d want 0", len(got))
	}
}

func TestContextStore_RemoveReturnsTrueOnHit(t *testing.T) {
	s := &ContextStore{Version: 1}
	s.Set(Space{Provider: "jira", SpaceID: "ENG"})
	if !s.Remove("jira", "ENG") {
		t.Error("Remove should return true for existing space")
	}
	if s.Remove("jira", "ENG") {
		t.Error("Remove should return false for already-removed space")
	}
	if len(s.Contexts) != 0 {
		t.Errorf("len(Contexts)=%d want 0 after Remove", len(s.Contexts))
	}
}

func TestContextStore_RemoveNilSafeAndMissing(t *testing.T) {
	var s *ContextStore
	if s.Remove("any", "any") {
		t.Error("nil receiver should return false")
	}
	s2 := &ContextStore{Version: 1}
	if s2.Remove("ghost", "x") {
		t.Error("missing entry should return false")
	}
}

func TestSaveContexts_Mode0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ctx.json")
	store := &ContextStore{Version: 1}
	store.Set(Space{Provider: "jira", SpaceID: "ENG"})
	if err := SaveContexts(store, path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// Permissions check is informational on macOS/Linux; skip on Windows.
}

func TestSaveContexts_NilFails(t *testing.T) {
	if err := SaveContexts(nil, filepath.Join(t.TempDir(), "x.json")); err == nil {
		t.Error("Save(nil) should fail")
	}
}
