package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type StressMetrics struct {
	Concurrency       int     `json:"concurrency"`
	RequestsPerWorker int     `json:"requests_per_worker"`
	TotalRequests     int     `json:"total_target_requests"`
	SuccessfulReqs    int     `json:"successful_reqs"`
	FailedReqs        int     `json:"failed_reqs"`
	DroppedByOS       int     `json:"dropped_by_os"`
	ElapsedSeconds    float64 `json:"elapsed_seconds"`
	RPS               float64 `json:"rps"`
}

func getTLSConfig(targetURL, workspace string) *tls.Config {
	if !strings.HasPrefix(targetURL, "https://") {
		//nolint:gosec // G402-STRESS-TEST: intentional for internal TLS stress testing
		return &tls.Config{InsecureSkipVerify: true}
	}

	cert, err := tls.LoadX509KeyPair(filepath.Join(workspace, ".neo/pki/client.crt"), filepath.Join(workspace, ".neo/pki/client.key"))
	if err != nil {
		log.Fatalf("[FATAL] Fallo cargando identidad cliente: %v", err)
	}

	caCert, err := os.ReadFile(filepath.Join(workspace, ".neo/pki/ca.crt"))
	if err != nil {
		log.Fatalf("[FATAL] Fallo cargando CA: %v", err)
	}
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)

	//nolint:gosec // G402-STRESS-TEST: intentional for internal TLS stress testing
	return &tls.Config{
		Certificates:       []tls.Certificate{cert},
		RootCAs:            caCertPool,
		InsecureSkipVerify: true,
	}
}

func runLoadTest(concurrency, requests int, targetURL string, client *http.Client) StressMetrics {
	payload := []byte(`{"MachineID":"PLC-01","Timestamp":1712070000,"State":3,"Payload":""}`)
	log.Printf("[STRESS] Iniciando asedio ininterrumpido: %d workers, %d requests totales sobre %s...", concurrency, concurrency*requests, targetURL)
	start := time.Now()

	var successfulReqs atomic.Uint64
	var failedReqs atomic.Uint64
	var droppedByOS atomic.Uint64
	var wg sync.WaitGroup

	for range concurrency {
		wg.Go(func() {

			// ZERO-ALLOCATION SRE: Rehusamos 1 solo lector por Worker en vez de asignar N.
			buf := bytes.NewReader(payload)
			drainBuf := make([]byte, 1024)

			for range requests {
				buf.Seek(0, 0)
				req, err := http.NewRequest(http.MethodPost, targetURL, buf)
				if err != nil {
					failedReqs.Add(1)
					runtime.Gosched()
					continue
				}

				resp, err := client.Do(req)
				if err != nil {
					failedReqs.Add(1)
					runtime.Gosched()
					continue
				}

				if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusAccepted {
					successfulReqs.Add(1)
				} else {
					failedReqs.Add(1)
					runtime.Gosched()
				}

				// ZERO-ALLOCATION / Socket Drain: Liberación pura del Body para reusar conexión TLS Mpx.
				if resp.Body != nil {
					for {
						_, err := resp.Body.Read(drainBuf)
						if err != nil {
							break
						}
					}
					resp.Body.Close()
				}
			}
		})
	}

	wg.Wait()
	elapsed := time.Since(start)
	totalSuccess := successfulReqs.Load()
	elapsedSecs := elapsed.Seconds()

	var rps float64
	if elapsedSecs > 0 {
		rps = float64(totalSuccess) / elapsedSecs
	}

	return StressMetrics{
		Concurrency:       concurrency,
		RequestsPerWorker: requests,
		TotalRequests:     concurrency * requests,
		SuccessfulReqs:    int(totalSuccess),
		FailedReqs:        int(failedReqs.Load()),
		DroppedByOS:       int(droppedByOS.Load()),
		ElapsedSeconds:    elapsedSecs,
		RPS:               rps,
	}
}

func main() {
	concurrency := flag.Int("c", 100, "Número de workers simultáneos (Goroutines)")
	requests := flag.Int("n", 10000, "Número de peticiones por worker")
	targetURL := flag.String("url", "https://localhost:8081/api/v1/telemetry", "URL del objetivo a estresar")
	flag.Parse()

	workspace, _ := os.Getwd()

	client := &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        *concurrency,
			MaxIdleConnsPerHost: *concurrency,
			IdleConnTimeout:     30 * time.Second,
			TLSClientConfig:     getTLSConfig(*targetURL, workspace),
		},
		Timeout: 5 * time.Second,
	}

	metrics := runLoadTest(*concurrency, *requests, *targetURL, client)

	outJSON, err := json.Marshal(metrics)
	if err != nil {
		log.Fatalf("[SRE-FATAL] Corrupción en serialización de métricas termodinámicas: %v", err)
	}
	log.Printf("%s\n", string(outJSON))
}
