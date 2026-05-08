package main

import (
	"log"
	"math/rand"
	"net/http"
	"time"
)

func main() {
	http.HandleFunc("/erp/ingest", func(w http.ResponseWriter, r *http.Request) {
		// Simulación de Caos: 70% de las peticiones fallarán con 503 Service Unavailable
		if rand.Float32() < 0.70 { //nolint:gosec // G402-STRESS-TEST
			log.Println("[CHAOS] ERP Ocupado / Caída de Red simulada. Devolviendo 503.")
			http.Error(w, "Chaos SRE 503: ERP Offline", http.StatusServiceUnavailable)
			return
		}

		// 30% de éxito (Happy Path)
		log.Println("[CHAOS] Conexión estable. Batch de telemetría ingerido (200 OK).")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ack"}`))
	})

	log.Println("[SRE] Iniciando Chaos ERP Simulator en http://localhost:9090")
	srv := &http.Server{
		Addr:         ":9090",
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}
