package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"go.etcd.io/bbolt"
	"github.com/ensamblatec/neoanvil/pkg/config"
	"github.com/ensamblatec/neoanvil/pkg/edgesync"
	"github.com/ensamblatec/neoanvil/pkg/mes"
	"github.com/ensamblatec/neoanvil/pkg/sre"
	"github.com/ensamblatec/neoanvil/pkg/wms"

	"github.com/ensamblatec/neoanvil/pkg/finops"
	"github.com/ensamblatec/neoanvil/pkg/memx"
	"github.com/ensamblatec/neoanvil/pkg/mesh"
	"github.com/ensamblatec/neoanvil/pkg/telemetry"
)

type SRELogWriter struct{}

func (w *SRELogWriter) Write(p []byte) (n int, err error) {
	telemetry.EmitEvent("FIREHOSE", string(p))
	return os.Stdout.Write(p)
}

func main() {
	log.SetOutput(&SRELogWriter{})
	log.Printf("[SANDBOX-INFO] 🚀 Arrancando Servidor Industrial Antigravity (SRE Decoupled Node)\\n")

	// Init Ficticio Múltiple (HAL Abstraction)
	rapl, _ := finops.MountRAPL()

	workspace, _ := os.Executable()
	for {
		if _, err := os.Stat(filepath.Join(workspace, "neo.yaml")); err == nil {
			break
		}
		parent := filepath.Dir(workspace)
		if parent == workspace {
			log.Fatalf("No se encontró neo.yaml")
		}
		workspace = parent
	}
	workspace, _ = filepath.Abs(workspace)

	cfgPath := filepath.Join(workspace, "neo.yaml")
	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		log.Fatalf("Fallo cargando neo.yaml: %v", err)
	}

	// PMEM Persistencia (Ahijado a la config SRE)
	pmemPath := filepath.Join(workspace, "brain.db")
	alloc, _ := memx.MountPMEM(pmemPath, 1024*1024*10, false)

	// XDP UMEM Dummy
	umem := make([]byte, 8192)
	xdp, err := mesh.MountAFXDP(umem, 64)
	if err != nil {
		log.Fatalf("[SRE-FATAL] Fallo acoplando AF_XDP: %v\n", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log.Println("[BOOT] ⚙️ Iniciando Secuencia de Planta Industrial (Modo GAMEDAY DEDICADO)")

	os.MkdirAll(filepath.Join(workspace, ".neo", "db"), 0755)
	dbOutbox, err := bbolt.Open(filepath.Join(workspace, ".neo", "db", "telemetry_outbox.db"), 0600, nil)
	if err != nil {
		log.Fatalf("Fallo abriendo DB de Outbox: %v", err)
	}
	defer dbOutbox.Close()

	cepEngine := mes.NewCEPEngine(5 * time.Second)
	cepEngine.UpdateRules(map[string]mes.CEPRule{
		"PLC-01": {
			TriggerState: 3,
			PLCEndpoint:  cfg.Integrations.PLCEndpoint,
			Payload:      []byte(`{"command":"e-stop"}`),
		},
	})
	mesDispatcher := mes.NewEventDispatcher(100000, dbOutbox, cepEngine)
	mesDispatcher.Start(ctx)
	defer mesDispatcher.Shutdown()

	wsHub := mes.NewHub(mesDispatcher)
	go wsHub.Start(ctx)

	resolvePath := func(path string) string {
		if filepath.IsAbs(path) {
			return path
		}
		return filepath.Join(workspace, path)
	}

	caPath := resolvePath(cfg.PKI.CACertPath)
	serverCertPath := resolvePath(cfg.PKI.ServerCertPath)
	serverKeyPath := resolvePath(cfg.PKI.ServerKeyPath)

	filesToVerify := []string{caPath, serverCertPath, serverKeyPath}
	for _, f := range filesToVerify {
		if _, err := os.Stat(f); os.IsNotExist(err) {
			log.Fatalf("[SRE-FATAL] Material mTLS faltante: %s", f)
		}
	}
	// [147.G] Enforce private-key file permissions — key must not be world/group readable.
	if fi, err := os.Stat(serverKeyPath); err == nil {
		if fi.Mode().Perm()&0o077 != 0 {
			if chmodErr := os.Chmod(serverKeyPath, 0o600); chmodErr != nil {
				log.Fatalf("[SRE-FATAL] Servidor private key %q tiene permisos inseguros (%s) y no se pudo corregir: %v",
					serverKeyPath, fi.Mode().Perm(), chmodErr)
			}
			log.Printf("[SANDBOX] private key %q permissions tightened to 0600", serverKeyPath)
		}
	}

	tlsConfig, err := sre.LoadMTLSConfig(caPath)
	if err != nil {
		log.Fatalf("[SRE-FATAL] No se pudo cargar mTLS: %v", err)
	}

	// [SRE] Disyuntor Termodinámico In-Flight (Límite: 100)
	meshRouter := mesh.NewRouter(100)

	// [SRE-HOTFIX] Snapshot Isolation: Enviamos solo la ruta en lugar de abrir el WAL
	// para evitar colisiones de LOCK_EX con el orquestador principal neo-mcp.
	ragWALPath := filepath.Join(workspace, cfg.RAG.DBPath)

	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.SandboxPort)
	if cfg.Server.Host == "" || cfg.Server.Host == "localhost" {
		addr = fmt.Sprintf(":%d", cfg.Server.SandboxPort)
	}
	mesServer := mes.NewIngestionServer(addr, mesDispatcher, wsHub, tlsConfig, meshRouter, ragWALPath)

	forwarder := edgesync.NewForwarder(dbOutbox, cfg.Integrations.ERPEndpoint)
	go forwarder.Start(ctx)

	go edgesync.StartRetentionSweeper(ctx, dbOutbox, 1000000)

	wmsDbPath := filepath.Join(workspace, ".neo", "db", "wms_idempotency.db")
	wmsDB, err := bbolt.Open(wmsDbPath, 0600, nil)
	if err != nil {
		log.Fatalf("[SRE-FATAL] failed to open WMS database: %v", err)
	}
	defer wmsDB.Close()

	go wms.StartBackgroundSweeper(ctx, wmsDB)

	mesServer.RegisterHandler("/api/v1/wms/scan", wms.IdempotencyMiddleware(wmsDB)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte("WMS Pallet Processed\n"))
		}),
	))

	tacticalAddr := fmt.Sprintf("127.0.0.1:%d", cfg.Server.TacticalPort)
	mesServer.Start(serverCertPath, serverKeyPath, cfg.Server.Tailscale, tacticalAddr)

	log.Println("[SANDBOX-INFO] Conectando Telemetría SRE al HUD Central...")
	telemetry.InitFirehose(ctx)
	sre.InitPulseEmitter()

	log.Printf("[SANDBOX-INFO] [SRE-Daemon] En línea. Esperando directiva térmica...\\n")
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	<-sigChan

	log.Printf("\\n[SANDBOX-INFO] [SRE-Daemon] 🛑 Directiva de Cesación Atrapada (SIGTERM/SIGINT). Ejecutando Graceful Teardown...\\n")
	cancel() // [SRE-FIX] Desalojar motor asíncrono para prevenir Deadlock LIFO en Defers

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer shutdownCancel()

	done := make(chan struct{})
	go func() {
		// Teardown Sequence
		log.Printf("[SANDBOX-INFO]   1. Apagando Telemetría SRE HTTP/TLS (8081)...\\n")
		mesServer.Shutdown(shutdownCtx)

		log.Printf("[SANDBOX-INFO]   2. Cerrando Tráfico Abrupto (AF_XDP Bypass)...\\n")
		if xdp != nil {
			xdp.Close()
		}

		log.Printf("[SANDBOX-INFO]   3. Acoplando flush Termodinámico Físico PMEM (NVMe DAX Sync)...\\n")
		if alloc != nil {
			// Flush naturo para evitar desvíos finales en DB
			alloc.Sync()
		}

		log.Printf("[SANDBOX-INFO]   4. Extirpación Térmica (Enfriado del Motor RAPL)...\\n")
		if rapl != nil {
			rapl.Close()
		}

		log.Printf("[SANDBOX-INFO] [SRE-Daemon] Orquestador apagado sin corrupción en silicio.\\n")
		close(done)
	}()

	select {
	case <-done:
		return
	case <-shutdownCtx.Done():
		log.Fatalf("[SANDBOX-FATAL] ⚠️ [SRE-VETO] El Teardown sobrepasó el Límite Crítico (Timeout). Forzando Caída.\\n")
	}

}
