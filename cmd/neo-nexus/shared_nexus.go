// cmd/neo-nexus/shared_nexus.go — Nexus-global KnowledgeStore owner. [354.Z-redesign]
//
// The Nexus dispatcher opens ~/.neo/shared/db/global.db and becomes the single
// owner of the Nexus-global tier. neo-mcp children proxy tier:"nexus" calls
// to these endpoints instead of opening their own handle (which hits bbolt's
// exclusive flock). This makes boot order irrelevant and keeps the nexus-
// global "god" always available — children just need Nexus to be alive.
//
// Endpoints (all POST, JSON body):
//   /api/v1/shared/nexus/store    — {namespace, key, content, tags, hot}
//   /api/v1/shared/nexus/fetch    — {namespace, key}
//   /api/v1/shared/nexus/list     — {namespace ('*' = all), tag}
//   /api/v1/shared/nexus/drop     — {namespace, key}
//   /api/v1/shared/nexus/search   — {namespace, query, k}
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/ensamblatec/neoanvil/pkg/knowledge"
	"github.com/ensamblatec/neoanvil/pkg/shared"
)

// nexusGlobalKS is the Nexus-owned handle to ~/.neo/shared/db/global.db.
// Opened at boot, closed on shutdown. nil when open failed — handlers return 503.
var nexusGlobalKS *knowledge.KnowledgeStore

// openNexusGlobalStore tries to acquire the flock on ~/.neo/shared/db/global.db.
// Called once at boot. Nexus is a singleton per installation — no contention
// expected in practice. If it fails, nexus-global endpoints return 503.
func openNexusGlobalStore() error {
	ks, err := shared.OpenGlobalStore()
	if err != nil {
		return fmt.Errorf("nexus-global: %w", err)
	}
	nexusGlobalKS = ks
	log.Printf("[NEXUS] nexus-global store opened (tier:\"nexus\" active via /api/v1/shared/nexus/*)")
	return nil
}

// closeNexusGlobalStore releases the flock. Deferred at shutdown.
func closeNexusGlobalStore() {
	if nexusGlobalKS != nil {
		_ = nexusGlobalKS.Close()
	}
}

// registerNexusSharedHandlers wires the tier:"nexus" endpoints on mux.
func registerNexusSharedHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/shared/nexus/store", handleNexusStore)
	mux.HandleFunc("/api/v1/shared/nexus/fetch", handleNexusFetch)
	mux.HandleFunc("/api/v1/shared/nexus/list", handleNexusList)
	mux.HandleFunc("/api/v1/shared/nexus/drop", handleNexusDrop)
	mux.HandleFunc("/api/v1/shared/nexus/search", handleNexusSearch)
}

// requireNexusGlobal rejects with 503 when the store failed to open at boot.
func requireNexusGlobal(w http.ResponseWriter) bool {
	if nexusGlobalKS == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "nexus-global store not available (dispatcher failed to open ~/.neo/shared/db/global.db)",
		})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

type nexusStoreReq struct {
	Namespace string   `json:"namespace"`
	Key       string   `json:"key"`
	Content   string   `json:"content"`
	Tags      []string `json:"tags,omitempty"`
	Hot       bool     `json:"hot,omitempty"`
}

func handleNexusStore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !requireNexusGlobal(w) {
		return
	}
	var req nexusStoreReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad JSON: " + err.Error()})
		return
	}
	if req.Namespace == "" || req.Key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "namespace and key are required"})
		return
	}
	e := knowledge.KnowledgeEntry{
		Key:       req.Key,
		Namespace: req.Namespace,
		Content:   req.Content,
		Tags:      req.Tags,
		Hot:       req.Hot,
	}
	if err := nexusGlobalKS.Put(req.Namespace, req.Key, e); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": req.Key, "namespace": req.Namespace})
}

type nexusFetchReq struct {
	Namespace string `json:"namespace"`
	Key       string `json:"key"`
}

func handleNexusFetch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !requireNexusGlobal(w) {
		return
	}
	var req nexusFetchReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad JSON: " + err.Error()})
		return
	}
	if req.Namespace == "" || req.Key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "namespace and key are required"})
		return
	}
	entry, err := nexusGlobalKS.Get(req.Namespace, req.Key)
	if err != nil || entry == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found", "namespace": req.Namespace, "key": req.Key})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entry": *entry})
}

type nexusListReq struct {
	Namespace string `json:"namespace"`
	Tag       string `json:"tag,omitempty"`
}

func handleNexusList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !requireNexusGlobal(w) {
		return
	}
	var req nexusListReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad JSON: " + err.Error()})
		return
	}
	if req.Namespace == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "namespace is required (use '*' for all)"})
		return
	}
	if req.Namespace == "*" {
		nss, err := nexusGlobalKS.ListNamespaces()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		type nsBucket struct {
			Namespace string                     `json:"namespace"`
			Entries   []knowledge.KnowledgeEntry `json:"entries"`
		}
		out := make([]nsBucket, 0, len(nss))
		for _, ns := range nss {
			entries, lErr := nexusGlobalKS.List(ns, req.Tag)
			if lErr != nil {
				continue
			}
			out = append(out, nsBucket{Namespace: ns, Entries: entries})
		}
		writeJSON(w, http.StatusOK, map[string]any{"namespaces": out})
		return
	}
	entries, err := nexusGlobalKS.List(req.Namespace, req.Tag)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"namespace": req.Namespace, "entries": entries})
}

type nexusDropReq struct {
	Namespace string `json:"namespace"`
	Key       string `json:"key"`
}

func handleNexusDrop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !requireNexusGlobal(w) {
		return
	}
	var req nexusDropReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad JSON: " + err.Error()})
		return
	}
	if req.Namespace == "" || req.Key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "namespace and key are required"})
		return
	}
	if err := nexusGlobalKS.Delete(req.Namespace, req.Key); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "dropped": req.Key, "namespace": req.Namespace})
}

type nexusSearchReq struct {
	Namespace string `json:"namespace"`
	Query     string `json:"query"`
	K         int    `json:"k,omitempty"`
}

func handleNexusSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !requireNexusGlobal(w) {
		return
	}
	var req nexusSearchReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad JSON: " + err.Error()})
		return
	}
	if req.Query == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "query is required"})
		return
	}
	if req.K <= 0 {
		req.K = 10
	}
	entries, err := nexusGlobalKS.Search(req.Namespace, req.Query, req.K)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"namespace": req.Namespace, "query": req.Query, "results": entries})
}
