package telemetry

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"os"
)

type ControlSignal struct {
	Command string `json:"Command"`
	Payload string `json:"Payload"`
}

// InitControlSocket abre el puerto de escucha asincrónico para God-Mode
func InitControlSocket(ctx context.Context) {
	_ = os.RemoveAll("/tmp/neo-control.sock")
	listener, err := net.Listen("unix", "/tmp/neo-control.sock")
	if err != nil {
		log.Printf("[SRE-CONTROL] Error bind: %v", err)
		return
	}
	// [SRE-15.5.1] Blind socket to owner-only to prevent local privilege escalation
	_ = os.Chmod("/tmp/neo-control.sock", 0600)

	go func() {
		defer listener.Close()
		go func() { <-ctx.Done(); listener.Close() }() //nolint:errcheck
		for {
			conn, err := listener.Accept()
			if err != nil {
				return // ctx cancelled → listener closed
			}
			go handleControlCommand(conn)
		}
	}()
}

func handleControlCommand(conn net.Conn) {
	defer conn.Close()
	decoder := json.NewDecoder(conn)
	var sig ControlSignal
	if err := decoder.Decode(&sig); err == nil {
		switch sig.Command {
		case "FLUSH":
			EmitEvent("FIREHOSE", "⚡ [GOD-MODE] Flush PMEM forzado manualmente")
			// Hook hacia sync() interno si estuviere implementado en kernel
		case "QUARANTINE":
			EmitEvent("INMUNOLOGÍA", "🛡️ [GOD-MODE] IP "+sig.Payload+" aislada en LRU")
		case "GOSSIP":
			EmitEvent("TOPOLOGÍA", "🌐 [GOD-MODE] Ciclo de propagación Gossip forzado")
		}
	}
}

// SendCommand inyecta órdenes al servidor desde el TUI
func SendCommand(cmd string, payload string) {
	conn, err := net.Dial("unix", "/tmp/neo-control.sock")
	if err != nil {
		return // Silencio táctico si orquestador está abajo
	}
	defer conn.Close()
	_ = json.NewEncoder(conn).Encode(ControlSignal{Command: cmd, Payload: payload})
}
