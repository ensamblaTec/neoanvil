package telemetry

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
)

var (
	TuiListener net.Listener
	tuiChan     = make(chan TelemetryMsg, 5000)
	listenerOn  bool
)

type TelemetryMsg struct {
	Quadrant string `json:"Quadrant"`
	Data     string `json:"Data"`
}

func startMultiClientListener() {
	_ = os.RemoveAll("/tmp/neo-telemetry.sock")
	l, err := net.Listen("unix", "/tmp/neo-telemetry.sock")
	if err != nil {
		log.Printf("[SRE-FATAL] TUI Listener no pudo bindear: %v", err)
		return
	}
	TuiListener = l
	defer func() {
		listenerOn = false
		_ = l.Close()
	}()
	for {
		conn, acceptErr := TuiListener.Accept()
		if acceptErr != nil {
			return // listener closed or fatal error — ListenSocket will restart on next call
		}
		go handleTuiClient(conn)
	}
}

func handleTuiClient(conn net.Conn) {
	defer conn.Close()
	decoder := json.NewDecoder(conn)
	for {
		var event struct {
			Quadrant string      `json:"Quadrant"`
			Data     interface{} `json:"Data"`
		}
		if err := decoder.Decode(&event); err != nil {
			return
		}
		strData := fmt.Sprintf("%v", event.Data)
		select {
		case tuiChan <- TelemetryMsg{Quadrant: event.Quadrant, Data: strData}:
		default: // Drop si TUI está colapsado CPU
		}
	}
}

func ListenSocket() TelemetryMsg {
	if !listenerOn {
		listenerOn = true
		go startMultiClientListener()
	}
	// Bloquea limpiamente hasta recibir del canal (BubbleTea lo ejecuta en hilo Cmd)
	msg := <-tuiChan
	return msg
}
