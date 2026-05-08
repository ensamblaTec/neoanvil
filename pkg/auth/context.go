package auth

// context.go — Active workspace/space tracking per provider. PILAR XXIII /
// Épica 124.8.
//
// Credentials answer "who am I" (email, token, domain). ContextStore
// answers "where am I working" — the active project / space / repo /
// channel for each provider. Persisted to ~/.neo/contexts.json (0600)
// alongside credentials.json.
//
// Generic terminology: a "Space" is whatever organizational unit the
// provider exposes — Jira project, Confluence space, GitHub repo,
// Linear team, Slack channel. Plugin authors map it to their target.
//
// At plugin spawn, the vault lookup combines CredEntry fields (token,
// email, domain) with the active Space (id, name) and injects them as
// env vars. The plugin reads them via os.Getenv.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Space is a single registered context within a provider. A Space holds the
// "where am I working" coordinates:
//
//   - SpaceID/Name : the parent container (Jira project, Confluence space,
//     GitHub repo, Linear team, Slack workspace).
//   - BoardID/Name : optional view inside the parent (Jira Kanban/Scrum
//     board, GitHub Projects board, Linear cycle). Many providers support
//     multiple boards per project; the active one is tracked here.
//
// Both BoardID and BoardName are optional — Confluence and similar
// products without a board concept simply leave them empty.
type Space struct {
	Provider  string `json:"provider"`
	SpaceID   string `json:"space_id"`
	SpaceName string `json:"space_name,omitempty"`
	BoardID   string `json:"board_id,omitempty"`
	BoardName string `json:"board_name,omitempty"`
	UpdatedAt string `json:"updated_at"`
}

// ContextStore is the top-level structure persisted to contexts.json.
// Active maps provider → space_id of the currently active Space for that
// provider. Contexts is the flat list of all registered spaces.
type ContextStore struct {
	Version  int               `json:"version"`
	Active   map[string]string `json:"active,omitempty"`
	Contexts []Space           `json:"contexts,omitempty"`
}

// DefaultContextsPath returns ~/.neo/contexts.json.
func DefaultContextsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".neo", "contexts.json")
}

// LoadContexts reads contexts.json. Returns an empty ContextStore (Version:1)
// if the file does not exist.
func LoadContexts(path string) (*ContextStore, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304-CLI-CONSENT: operator-managed contexts file
	if err != nil {
		if os.IsNotExist(err) {
			return &ContextStore{Version: 1, Active: map[string]string{}}, nil
		}
		return nil, err
	}
	var store ContextStore
	if err := json.Unmarshal(data, &store); err != nil {
		return nil, err
	}
	if store.Active == nil {
		store.Active = map[string]string{}
	}
	return &store, nil
}

// SaveContexts writes contexts.json with 0600 permissions, creating parent
// dirs as needed.
func SaveContexts(s *ContextStore, path string) error {
	if s == nil {
		return fmt.Errorf("nil ContextStore")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // G304-CLI-CONSENT: operator-managed contexts path
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}

// Set upserts a Space keyed by (provider, space_id). Stamps UpdatedAt.
// Does not change the active marker.
func (s *ContextStore) Set(sp Space) {
	if sp.UpdatedAt == "" {
		sp.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	for i, existing := range s.Contexts {
		if existing.Provider == sp.Provider && existing.SpaceID == sp.SpaceID {
			s.Contexts[i] = sp
			return
		}
	}
	s.Contexts = append(s.Contexts, sp)
}

// Use marks a Space as active for its provider. The Space must already be
// registered via Set; returns an error otherwise so typos surface
// immediately rather than silently activating nothing.
func (s *ContextStore) Use(provider, spaceID string) error {
	if strings.TrimSpace(provider) == "" {
		return fmt.Errorf("provider is required")
	}
	if strings.TrimSpace(spaceID) == "" {
		return fmt.Errorf("space_id is required")
	}
	for _, sp := range s.Contexts {
		if sp.Provider == provider && sp.SpaceID == spaceID {
			if s.Active == nil {
				s.Active = map[string]string{}
			}
			s.Active[provider] = spaceID
			return nil
		}
	}
	return fmt.Errorf("space %q for provider %q not found — register it first", spaceID, provider)
}

// ActiveSpace returns the active Space for the given provider, or nil when
// none is set or the registered active points at a now-removed space.
// Named ActiveSpace (not Active) so it doesn't collide with the JSON field.
func (s *ContextStore) ActiveSpace(provider string) *Space {
	if s == nil || s.Active == nil {
		return nil
	}
	id, ok := s.Active[provider]
	if !ok {
		return nil
	}
	for i := range s.Contexts {
		if s.Contexts[i].Provider == provider && s.Contexts[i].SpaceID == id {
			return &s.Contexts[i]
		}
	}
	return nil
}

// ListByProvider returns all registered Spaces for one provider, in
// insertion order.
func (s *ContextStore) ListByProvider(provider string) []Space {
	if s == nil {
		return nil
	}
	out := make([]Space, 0, len(s.Contexts))
	for _, sp := range s.Contexts {
		if sp.Provider == provider {
			out = append(out, sp)
		}
	}
	return out
}

// Remove deletes a Space and clears the active marker if it pointed there.
// Returns true when a Space was removed.
func (s *ContextStore) Remove(provider, spaceID string) bool {
	if s == nil {
		return false
	}
	for i, sp := range s.Contexts {
		if sp.Provider == provider && sp.SpaceID == spaceID {
			s.Contexts = append(s.Contexts[:i], s.Contexts[i+1:]...)
			if s.Active != nil && s.Active[provider] == spaceID {
				delete(s.Active, provider)
			}
			return true
		}
	}
	return false
}
