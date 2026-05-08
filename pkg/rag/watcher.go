package rag

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/fsnotify/fsnotify"
)

// [SRE-22.2.1] ValidateFunc is a callback invoked before indexing a mutated file.
// Returns nil if the file passes validation, error otherwise.
type ValidateFunc func(filename string) error

type AutoIndexer struct {
	workspace    string
	lexicalIdx   *LexicalIndex
	mu           sync.Mutex
	stop         chan struct{}
	watcher      *fsnotify.Watcher
	ignoreDirs   []string
	allowedExt   []string
	jobs         chan<- string
	validateFunc ValidateFunc // [SRE-22.2.1] Pre-index validation hook
}

func NewAutoIndexer(workspace string, lexicalIdx *LexicalIndex, ignoreDirs []string, allowedExt []string, jobs chan<- string, opts ...ValidateFunc) (*AutoIndexer, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	var vf ValidateFunc
	if len(opts) > 0 {
		vf = opts[0]
	}
	return &AutoIndexer{
		workspace:    workspace,
		lexicalIdx:   lexicalIdx,
		stop:         make(chan struct{}),
		watcher:      watcher,
		ignoreDirs:   ignoreDirs,
		allowedExt:   allowedExt,
		jobs:         jobs,
		validateFunc: vf,
	}, nil
}

func (ai *AutoIndexer) Start(ctx context.Context) {
	// Initial walk to add all directories to watcher
	err := filepath.WalkDir(ai.workspace, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			for _, ignore := range ai.ignoreDirs {
				if name == ignore || name == ".neo" {
					return filepath.SkipDir
				}
			}
			if err := ai.watcher.Add(path); err != nil {
				log.Printf("[SRE-WARN] failed to watch dir %s: %v", path, err)
			}
		}
		return nil
	})
	if err != nil {
		log.Printf("[SRE-ERROR] watcher walk failed: %v", err)
	}

	go func() {
		for {
			select {
			case <-ctx.Done():
				ai.watcher.Close()
				return
			case <-ai.stop:
				ai.watcher.Close()
				return
			case event, ok := <-ai.watcher.Events:
				if !ok {
					return
				}
				// [SRE-9.1.1] Reinforce fsnotify: capture Write and Create events
				if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
					info, err := os.Stat(event.Name)
					if err != nil {
						continue
					}
					if info.IsDir() {
						// Add new directories to watcher
						_ = ai.watcher.Add(event.Name)
						continue
					}

					ext := filepath.Ext(event.Name)
					supported := false
					for _, a := range ai.allowedExt {
						if ext == a {
							supported = true
							break
						}
					}

					if supported {
						// [SRE-22.2.2] Validate before indexing if hook is set
						if ai.validateFunc != nil {
							if err := ai.validateFunc(event.Name); err != nil {
								log.Printf("[SRE-22.2.2] Passive validation FAILED for %s: %v — blocking RAG indexing", event.Name, err)
								continue
							}
						}
						// Send to jobs channel for async embedding and indexing
						select {
						case ai.jobs <- event.Name:
						default:
							// Channel full, drop or log (SRE: prioritize stability)
						}
					}
				}
			case err, ok := <-ai.watcher.Errors:
				if !ok {
					return
				}
				log.Printf("[SRE-WARN] fsnotify watcher error: %v", err)
			}
		}
	}()
}

func (ai *AutoIndexer) Stop() {
	close(ai.stop)
}
