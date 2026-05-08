// cmd/neo-nexus/api_async.go — ÉPICA 376.F: REST endpoints for async
// task inspection. Loopback-only (inherits Nexus bind_addr restriction).
//
//	GET /api/v1/async/tasks?plugin=deepseek&status=done  → list
//	GET /api/v1/async/tasks/{id}                         → single task

package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

func handleAsyncTaskList(rt *pluginRuntime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if rt == nil || rt.asyncStore == nil {
			http.Error(w, "async store not available", http.StatusServiceUnavailable)
			return
		}
		pluginFilter := r.URL.Query().Get("plugin")
		statusFilter := AsyncTaskStatus(r.URL.Query().Get("status"))
		tasks, err := rt.asyncStore.List(pluginFilter, statusFilter)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(tasks)
	}
}

func handleAsyncTaskGet(rt *pluginRuntime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if rt == nil || rt.asyncStore == nil {
			http.Error(w, "async store not available", http.StatusServiceUnavailable)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/async/tasks/")
		if id == "" {
			http.Error(w, "missing task id", http.StatusBadRequest)
			return
		}
		task, err := rt.asyncStore.Get(id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(task)
	}
}
