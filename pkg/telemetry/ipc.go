package telemetry

import (
	"context"
	"encoding/json"
	"net"
	"time"
)

type FirehoseEvent struct {
	Quadrant string      `json:"Quadrant"` // TOPOLOGÍA, TERMODINÁMICA, INMUNOLOGÍA, FIREHOSE
	Data     interface{} `json:"Data"`
}

var eventChannel = make(chan FirehoseEvent, 2048)
var firehose net.Conn

// InitFirehose abre el canal oscuro hacia el TUI y levanta el consumer asíncrono con reconexión infinita.
func InitFirehose(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			conn, err := net.Dial("unix", "/tmp/neo-telemetry.sock")
			if err == nil {
				firehose = conn
				encoder := json.NewEncoder(conn)
				for ev := range eventChannel {
					if err := encoder.Encode(ev); err != nil {
						// Socket roto (TUI reiniciado), salimos del for interno para reconectar
						conn.Close()
						break
					}
				}
				conn.Close()
			}
			// Si no hay TUI, drena para evitar OOM
			select {
			case <-eventChannel:
			default:
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second): // Reintento SRE
			}
		}
	}()
}

// EmitEvent es O(1) puro. Si falla, el paquete muere en silencio (No Blocking).
func EmitEvent(quadrant string, payload interface{}) {
	select {
	case eventChannel <- FirehoseEvent{Quadrant: quadrant, Data: payload}:
	default:
		// SRE DROP: NUNCA bloquear el hilo principal de la IA.
	}
}
