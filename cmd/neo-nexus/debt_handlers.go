package main

// debt_handlers.go — /internal/nexus/debt HTTP endpoints exposing the
// DebtRegistry owned by the ProcessPool. [PILAR LXVI / 351.B]

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/ensamblatec/neoanvil/pkg/nexus"
)

// registerNexusDebtHandlers wires 3 endpoints:
//
//	GET  /internal/nexus/debt?filter=open&priority=P0&since=<unix>
//	     → JSON array of NexusDebtEvent
//	POST /internal/nexus/debt/resolve
//	     body: {"id":"YYYY-MM-DD-xxxx","resolution":"fixed"}
//	     → 200 (requires X-Nexus-Token when cfg.Nexus.API.AuthToken set)
//	GET  /internal/nexus/debt/affecting?workspace_id=<id>
//	     → JSON array of open events whose AffectedWorkspaces include wsID
//
// When pool.Debt is nil (debt.enabled:false in nexus.yaml), every handler
// responds with 404 + `{"error":"nexus debt disabled"}` so callers can
// distinguish "registry off" from "no events".
func registerNexusDebtHandlers(mux *http.ServeMux, pool *nexus.ProcessPool, cfg *nexus.NexusConfig) {
	authToken := ""
	if cfg != nil {
		authToken = cfg.Nexus.API.AuthToken
	}

	mux.HandleFunc("/internal/nexus/debt", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if pool == nil || pool.Debt == nil {
			writeDebtDisabled(w)
			return
		}
		filter := parseDebtFilter(r)
		events := pool.Debt.ListOpen(filter)
		writeDebtJSON(w, events)
	})

	mux.HandleFunc("/internal/nexus/debt/resolve", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !checkDebtAuth(r, authToken) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if pool == nil || pool.Debt == nil {
			writeDebtDisabled(w)
			return
		}
		var body struct {
			ID         string `json:"id"`
			Resolution string `json:"resolution"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
			http.Error(w, "bad request — expected {id,resolution}", http.StatusBadRequest)
			return
		}
		switch err := pool.Debt.ResolveDebt(body.ID, body.Resolution); err {
		case nil:
			log.Printf("[NEXUS-DEBT] resolved id=%s note=%q", body.ID, body.Resolution)
			writeDebtJSON(w, map[string]string{"resolved": body.ID})
		case nexus.ErrDebtNotFound:
			http.Error(w, "not found", http.StatusNotFound)
		case nexus.ErrDebtAlreadyResolved:
			http.Error(w, "already resolved", http.StatusConflict)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

	mux.HandleFunc("/internal/nexus/debt/affecting", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if pool == nil || pool.Debt == nil {
			writeDebtDisabled(w)
			return
		}
		wsID := r.URL.Query().Get("workspace_id")
		if wsID == "" {
			http.Error(w, "workspace_id required", http.StatusBadRequest)
			return
		}
		events := pool.Debt.Affecting(wsID)
		writeDebtJSON(w, events)
	})
}

func parseDebtFilter(r *http.Request) nexus.DebtFilter {
	q := r.URL.Query()
	f := nexus.DebtFilter{
		Priority:        strings.ToUpper(strings.TrimSpace(q.Get("priority"))),
		AffectedWS:      q.Get("workspace_id"),
		IncludeResolved: q.Get("filter") == "all",
	}
	if since := q.Get("since"); since != "" {
		if n, err := strconv.ParseInt(since, 10, 64); err == nil {
			f.SinceUnix = n
		}
	}
	return f
}

func checkDebtAuth(r *http.Request, configured string) bool {
	if configured == "" {
		return true
	}
	return r.Header.Get("X-Nexus-Token") == configured
}

func writeDebtJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("[NEXUS-DEBT] encode error: %v", err)
	}
}

func writeDebtDisabled(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"error":"nexus debt disabled"}`))
}
