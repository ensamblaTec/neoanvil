package sre

import (
	"context"
	"expvar"
	"log"
	"net/http"
	"net/http/pprof"
	"runtime"
	"sync/atomic"
	"time"
)

// Métricas atómicas globales (Zero-Allocation Counters)
var (
	MetricsMESIngested atomic.Uint64
	MetricsMESShedded  atomic.Uint64
	MetricsSyncSuccess atomic.Uint64
	MetricsSyncFailed  atomic.Uint64
)

// init registra las métricas en el expvar predeterminado de Go
func init() {
	expvar.Publish("mes_ingested_total", expvar.Func(func() any { return MetricsMESIngested.Load() }))
	expvar.Publish("mes_shedded_total", expvar.Func(func() any { return MetricsMESShedded.Load() }))
	expvar.Publish("sync_success_total", expvar.Func(func() any { return MetricsSyncSuccess.Load() }))
	expvar.Publish("sync_failed_total", expvar.Func(func() any { return MetricsSyncFailed.Load() }))
	expvar.Publish("goroutines_active", expvar.Func(func() any { return runtime.NumGoroutine() }))
}

// DiagnosticsServer encapsula el router de telemetría interna.
type DiagnosticsServer struct {
	srv *http.Server
}

// NewDiagnosticsServer inicializa el servidor en un puerto seguro (ej. 127.0.0.1:6060).
func NewDiagnosticsServer(addr string) *DiagnosticsServer {
	mux := http.NewServeMux()

	// 1. Endpoints de Profiling (CPU, Memoria, Goroutines, Mutex Contention)
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	// 2. Variables expuestas en JSON (Para ser consumidas por un scraper ligero)
	mux.Handle("/debug/vars", expvar.Handler())

	// 3. [PILAR LXVIII / 359.A] /metrics — Prometheus text exposition format.
	// Envuelve los expvar counters como métricas Prometheus estándar.
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		WritePrometheusMetrics(w)
	})

	return &DiagnosticsServer{
		srv: &http.Server{
			Addr:         addr,
			Handler:      mux,
			ReadTimeout:  5 * time.Second,
			WriteTimeout: 60 * time.Second, // pprof.Profile bloquea por 30s por defecto
		},
	}
}

// Start levanta el servidor de diagnóstico de forma asíncrona.
// Start levanta el servidor de diagnóstico de forma asíncrona.
func (d *DiagnosticsServer) Start() {
	log.Printf("[SRE] Servidor de Diagnóstico y Profiling iniciado en %s", d.srv.Addr)
	go func() {
		if err := d.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[SRE-WARN] Fallo en el servidor de diagnóstico SRE (Puerto %s posiblemente ocupado): %v. Saltando profiling.", d.srv.Addr, err)
		}
	}()
}

// Shutdown apaga el servidor elegantemente.
func (d *DiagnosticsServer) Shutdown(ctx context.Context) error {
	return d.srv.Shutdown(ctx)
}
