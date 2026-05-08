package sre

import (
	"encoding/binary"
	"fmt"
	"log"

	"github.com/cilium/ebpf/ringbuf"
	"github.com/ensamblatec/neoanvil/pkg/state"
)

type EBpfTracer struct {
	reader   *ringbuf.Reader
	stopChan chan struct{}
}

func NewEBpfTracer(reader *ringbuf.Reader) *EBpfTracer {
	return &EBpfTracer{
		reader:   reader,
		stopChan: make(chan struct{}),
	}
}

func (t *EBpfTracer) Start() {
	if t.reader == nil {
		log.Println("[SRE-BPF] Lector de eBPF inactivo, omitiendo rastreo")
		return
	}

	log.Println("[SRE-BPF] Goroutine de curación eBPF inicializada (Zero-Alloc)")
	go func() {
		for {
			record, err := t.reader.Read()
			if err != nil {
				select {
				case <-t.stopChan:
					return
				default:
					continue
				}
			}

			// Si leemos payload del núcleo que indica un salto de red TCP
			// Offset 8: PID (4 bytes), Offset 24: Retransmits (4 bytes)
			if len(record.RawSample) >= 28 {
				pid := binary.LittleEndian.Uint32(record.RawSample[8:12])
				retransmits := binary.LittleEndian.Uint32(record.RawSample[24:28])

				log.Printf("[BPF] Intercepción TCP SRE: Socket colgado detectado.")
				log.Printf("[SRE-FATAL] HANG DE RED NÚCLEO - PID: %d - Retransmits: %d", pid, retransmits)

				if err := state.EnqueueTasks([]state.SRETask{
					{Description: fmt.Sprintf("[SRE KERNEL INCIDENT] Anomalía de latencia física identificada vía eBPF en PID %d", pid), TargetFile: "NUCLEO SISTEMA"},
				}); err != nil {
					log.Printf("[SRE] Enqueue error: %v", err)
				}
			}
		}
	}()
}

func (t *EBpfTracer) Stop() {
	close(t.stopChan)
	if t.reader != nil {
		if err := t.reader.Close(); err != nil {
			log.Printf("[SRE] lector cerrado error %v", err)
		}
	}
}
