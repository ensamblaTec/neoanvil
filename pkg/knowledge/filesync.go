// pkg/knowledge/filesync.go — Dual-layer export/import + fsnotify watcher. [PILAR XXXIX / Épica 297]
//
// Each KnowledgeEntry has a companion .md file in .neo-project/knowledge/<ns>/<key>.md
// with YAML frontmatter. Changes to the files are reflected in BoltDB + HotCache within
// ~2 seconds via an fsnotify dir-watch (robust to sed -i rename-replace).
package knowledge

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

const frontmatterDelim = "---"

// EnsureSyncDir creates dir (and all parents) and places a .gitkeep so that an
// empty knowledge/ directory can be committed to git. Idempotent: safe to call
// on every boot. [297.F]
//
// [330.J] Also creates one subdirectory per ReservedNamespaces() entry with its
// own .gitkeep so agents see the full namespace layout — `debt`, `decisions`,
// `contracts`, etc. — from first boot. Previously `.neo-project/knowledge/debt/`
// only existed after the first Put, causing operators to believe the namespace
// was unsupported.
func EnsureSyncDir(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("knowledge: ensure sync dir: %w", err)
	}
	gitkeep := filepath.Join(dir, ".gitkeep")
	if _, err := os.Stat(gitkeep); os.IsNotExist(err) {
		if werr := os.WriteFile(gitkeep, nil, 0o640); werr != nil { //nolint:gosec // G304-DIR-WALK: fixed filename under controlled sync dir
			return werr
		}
	}
	// [330.J] Ensure each reserved namespace subdir exists with its own .gitkeep.
	for _, ns := range ReservedNamespaces() {
		nsDir := filepath.Join(dir, ns)
		if err := os.MkdirAll(nsDir, 0o750); err != nil {
			return fmt.Errorf("knowledge: ensure ns %s: %w", ns, err)
		}
		nsKeep := filepath.Join(nsDir, ".gitkeep")
		if _, err := os.Stat(nsKeep); os.IsNotExist(err) {
			if werr := os.WriteFile(nsKeep, nil, 0o640); werr != nil { //nolint:gosec // G304-DIR-WALK: fixed filename under controlled sync dir
				return fmt.Errorf("knowledge: ns gitkeep %s: %w", ns, werr)
			}
		}
	}
	return nil
}

// ExportEntry writes entry to <dir>/<namespace>/<key>.md. [297.A]
// Creates the directory if needed. Idempotent: skips the write when the file
// already contains identical content — this breaks the watcher→Put→ExportEntry
// re-entry loop that would otherwise fire on every fsnotify event. [297.C]
func ExportEntry(dir string, e KnowledgeEntry) error {
	nsDir := filepath.Join(dir, e.Namespace)
	if err := os.MkdirAll(nsDir, 0o750); err != nil {
		return fmt.Errorf("knowledge: export mkdir: %w", err)
	}
	fname := filepath.Join(nsDir, safeFilename(e.Key)+".md")
	var buf bytes.Buffer
	buf.WriteString(frontmatterDelim + "\n")
	fmt.Fprintf(&buf, "key: %s\n", e.Key)
	fmt.Fprintf(&buf, "namespace: %s\n", e.Namespace)
	fmt.Fprintf(&buf, "hot: %v\n", e.Hot)
	if len(e.Tags) > 0 {
		fmt.Fprintf(&buf, "tags: [%s]\n", strings.Join(e.Tags, ", "))
	}
	fmt.Fprintf(&buf, "updated_at: %s\n", time.Unix(e.UpdatedAt, 0).UTC().Format(time.RFC3339))
	buf.WriteString(frontmatterDelim + "\n")
	buf.WriteString(e.Content)
	newContent := buf.Bytes()
	// Skip write if file already has identical content (prevents watcher loop).
	if existing, err := os.ReadFile(fname); err == nil && bytes.Equal(existing, newContent) { //nolint:gosec // G304-DIR-WALK: fname derived from controlled dir arg
		return nil
	}
	return os.WriteFile(fname, newContent, 0o640) //nolint:gosec // G304-DIR-WALK: fname derived from controlled dir arg
}

// ImportEntry parses a .md file with YAML frontmatter and returns a KnowledgeEntry. [297.B]
func ImportEntry(path string) (*KnowledgeEntry, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304-DIR-WALK: path from fsnotify watcher over controlled dir
	if err != nil {
		return nil, err
	}
	content := string(data)
	// Split on first and second "---" delimiters.
	parts := strings.SplitN(content, frontmatterDelim+"\n", 3)
	if len(parts) < 3 {
		return nil, fmt.Errorf("knowledge: import: missing frontmatter in %s", path)
	}
	fm := parts[1]
	body := parts[2]

	e := &KnowledgeEntry{Content: strings.TrimRight(body, "\n")}
	for line := range strings.SplitSeq(fm, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		parseFrontmatterLine(e, k, v)
	}
	if e.Key == "" {
		e.Key = strings.TrimSuffix(filepath.Base(path), ".md")
	}
	if e.Namespace == "" {
		e.Namespace = filepath.Base(filepath.Dir(path))
	}
	return e, nil
}

// parseFrontmatterLine applies a single key:value pair from YAML frontmatter to e.
func parseFrontmatterLine(e *KnowledgeEntry, k, v string) {
	switch k {
	case "key":
		e.Key = v
	case "namespace":
		e.Namespace = v
	case "hot":
		e.Hot = v == "true"
	case "tags":
		v = strings.Trim(v, "[]")
		for t := range strings.SplitSeq(v, ",") {
			if tag := strings.TrimSpace(t); tag != "" {
				e.Tags = append(e.Tags, tag)
			}
		}
	case "updated_at":
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			e.UpdatedAt = t.Unix()
		}
	}
}

// BootstrapFromFiles imports all .md files from dir into ks+hc when BoltDB is empty. [297.E]
// Safe to call on every boot — Put is idempotent for unchanged entries.
func BootstrapFromFiles(dir string, ks *KnowledgeStore, hc *HotCache) error {
	return filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		e, err := ImportEntry(path)
		if err != nil {
			log.Printf("[297.E] knowledge bootstrap skip %s: %v", path, err)
			return nil
		}
		if putErr := ks.Put(e.Namespace, e.Key, *e); putErr != nil {
			log.Printf("[297.E] knowledge bootstrap put %s/%s: %v", e.Namespace, e.Key, putErr)
			return nil
		}
		if e.Hot {
			hc.Set(*e)
		}
		return nil
	})
}

// StartWatcher starts an fsnotify watcher over dir, syncing changes to ks+hc. [297.D]
// Returns a stop function. Non-blocking — runs in a goroutine.
func StartWatcher(dir string, ks *KnowledgeStore, hc *HotCache) (stop func(), err error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("knowledge: watcher: %w", err)
	}
	// Watch top-level dir + existing namespace subdirs.
	dirs, _ := filepath.Glob(filepath.Join(dir, "*"))
	_ = w.Add(dir)
	for _, d := range dirs {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			_ = w.Add(d)
		}
	}

	stopCh := make(chan struct{})
	go func() {
		defer w.Close()
		for {
			select {
			case <-stopCh:
				return
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				handleFSEvent(ev, w, ks, hc)
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				log.Printf("[297.D] knowledge watcher error: %v", err)
			}
		}
	}()
	return func() { close(stopCh) }, nil
}

// writeMu is a per-key mutex map guarding concurrent Put/Delete via the
// watcher against a simultaneous Agent Put. Both paths can fire on the same
// key within milliseconds (agent writes .md via ExportEntry → fsnotify event
// races with a concurrent human edit on the same file). Serializing per-key
// plus an UpdatedAt staleness check prevents a stale write from clobbering
// a fresher one. [344.A]
var writeMu sync.Map // key: "<ns>/<key>" → *sync.Mutex

func lockForKey(ns, key string) *sync.Mutex {
	id := ns + "/" + key
	if v, ok := writeMu.Load(id); ok {
		return v.(*sync.Mutex)
	}
	m := &sync.Mutex{}
	actual, _ := writeMu.LoadOrStore(id, m)
	return actual.(*sync.Mutex)
}

func handleFSEvent(ev fsnotify.Event, w *fsnotify.Watcher, ks *KnowledgeStore, hc *HotCache) {
	if ev.Has(fsnotify.Create) {
		if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
			_ = w.Add(ev.Name) // new namespace dir — start watching it
			return
		}
	}
	if !strings.HasSuffix(ev.Name, ".md") {
		return
	}
	if ev.Has(fsnotify.Write) || ev.Has(fsnotify.Create) {
		applyFSWrite(ev.Name, ks, hc)
		return
	}
	if ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename) {
		applyFSRemove(ev.Name, ks, hc)
	}
}

// applyFSWrite imports the .md at path and performs the gated Put. [344.A]
func applyFSWrite(path string, ks *KnowledgeStore, hc *HotCache) {
	e, err := ImportEntry(path)
	if err != nil {
		log.Printf("[297.D] knowledge watcher import %s: %v", path, err)
		return
	}
	mu := lockForKey(e.Namespace, e.Key)
	mu.Lock()
	defer mu.Unlock()
	if isStaleWrite(ks, e) {
		return
	}
	if err := ks.Put(e.Namespace, e.Key, *e); err != nil {
		log.Printf("[297.D] knowledge watcher put %s/%s: %v", e.Namespace, e.Key, err)
		return
	}
	hc.Set(*e)
	log.Printf("[297.D] knowledge watcher synced %s/%s (hot=%v)", e.Namespace, e.Key, e.Hot)
}

// isStaleWrite reports whether e's UpdatedAt is older than the existing
// store entry — in which case the Put should be skipped. Zero UpdatedAt
// falls through as non-stale (first-time sync or legacy entry). [344.A]
func isStaleWrite(ks *KnowledgeStore, e *KnowledgeEntry) bool {
	if e.UpdatedAt <= 0 {
		return false
	}
	existing, err := ks.Get(e.Namespace, e.Key)
	if err != nil || existing == nil {
		return false
	}
	if existing.UpdatedAt <= e.UpdatedAt {
		return false
	}
	log.Printf("[344.A] stale-write skipped %s/%s (incoming=%d < existing=%d)",
		e.Namespace, e.Key, e.UpdatedAt, existing.UpdatedAt)
	return true
}

// applyFSRemove handles Remove/Rename events derived from the watcher. [344.A]
func applyFSRemove(path string, ks *KnowledgeStore, hc *HotCache) {
	base := strings.TrimSuffix(filepath.Base(path), ".md")
	ns := filepath.Base(filepath.Dir(path))
	mu := lockForKey(ns, base)
	mu.Lock()
	defer mu.Unlock()
	if err := ks.Delete(ns, base); err != nil {
		log.Printf("[297.D] knowledge watcher delete %s/%s: %v", ns, base, err)
	}
	hc.Delete(ns, base)
	log.Printf("[297.D] knowledge watcher removed %s/%s", ns, base)
}

// safeFilename replaces characters that are invalid in filenames with underscores.
func safeFilename(key string) string {
	var b strings.Builder
	for _, r := range key {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
