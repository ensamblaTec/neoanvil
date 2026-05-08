package main

import (
	"log"
	"math/rand/v2"
	"net/http"
	"time"
)

func main() {
	http.HandleFunc("/plc/stop", func(w http.ResponseWriter, r *http.Request) {
		// Simular el PLC recibiendo la orden. Un PLC bajo estrés o roto podría fallar (20% de veces fallará red local)
		//nolint:gosec // G402-STRESS-TEST
		if rand.Float32() < 0.20 {
			log.Println("[PLC-CHAOS] PLC Dead. Omitiendo conexión OT.")
			time.Sleep(100 * time.Millisecond) // Forza Timeouts SRE
			return
		}

		log.Println("[PLC-CHAOS] PLC detuvo los rotores con éxito (E-Stop Acatado: 200 OK).")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"plc_status":"halt"}`))
	})

	log.Println("[SRE-PLC] Iniciando simulador de PLC esclavo en http://localhost:9091")
	srv := &http.Server{
		Addr:         ":9091",
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}
