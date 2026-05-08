package telemetry

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"os"
	"time"
)

func StartTelemetryServer(ctx context.Context, socketPath string) {
	log.Printf("[BOOT] Iniciando servidor de Telemetría IPC Sockets...")
	os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		log.Printf("[SRE-FATAL] No se pudo unir el socket de telemetría: %v", err)
		return
	}
	// [SRE-15.5.1] Restrict socket to owner-only to prevent privilege escalation
	_ = os.Chmod(socketPath, 0600)
	defer listener.Close()
	go func() { <-ctx.Done(); listener.Close() }() //nolint:errcheck

	// Zero-Allocation IPC Mux Multiplexing
	for {
		conn, connErr := listener.Accept()
		if connErr != nil {
			return // ctx cancelled → listener closed → Accept fails
		}

		go func(c net.Conn) {
			defer c.Close()
			// Creamos el encoder una sola vez por handshake TCP
			encoder := json.NewEncoder(c)
			for {
				snapshot := GetSystemStats()

				if err := encoder.Encode(snapshot); err != nil {
					return // Cliente desconectado, termina goroutine de forma limpia SIN leaking.
				}
				time.Sleep(100 * time.Millisecond) // Tick Rate (HUD)
			}
		}(conn)
	}
}
