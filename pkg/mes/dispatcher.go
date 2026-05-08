package mes

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"go.etcd.io/bbolt"
	"github.com/ensamblatec/neoanvil/pkg/sre"
)

// TelemetryEvent representa una lectura de piso de planta.
type TelemetryEvent struct {
	MachineID string
	Timestamp int64
	State     int // Ej: 1=Running, 2=Downtime, 3=Fault
	Payload   []byte
}

type EventDispatcher struct {
	ingestCh   chan TelemetryEvent
	dbBatchCh  chan TelemetryEvent
	wsHubCh    chan TelemetryEvent
	wg         sync.WaitGroup
	bufferSize int
	db         *bbolt.DB  // Dependencia Inyectada
	cep        *CEPEngine // Motor CEP SRE Inyectado
}

// keyPool mitiga Thrashing de GC (Zero-Allocation primaria)
var keyPool = sync.Pool{
	New: func() any {
		b := make([]byte, 128)
		return &b
	},
}

// NewEventDispatcher inicializa los canales con backpressure táctico.
func NewEventDispatcher(bufferSize int, db *bbolt.DB, cep *CEPEngine) *EventDispatcher {
	_ = db.Update(func(tx *bbolt.Tx) error {
		_, _ = tx.CreateBucketIfNotExists([]byte("sync_outbox"))
		return nil
	})

	return &EventDispatcher{
		ingestCh:   make(chan TelemetryEvent, bufferSize),
		dbBatchCh:  make(chan TelemetryEvent, bufferSize),
		wsHubCh:    make(chan TelemetryEvent, bufferSize),
		bufferSize: bufferSize,
		db:         db,
		cep:        cep,
	}
}

// Start levanta el motor de ruteo.
func (d *EventDispatcher) Start(ctx context.Context) {
	d.wg.Go(func() {
		for {
			select {
			case <-ctx.Done():
				log.Println("[SRE] Dispatcher apagándose. Drenando canales...")
				return
			case ev := <-d.ingestCh:
				// Fan-Out con semántica Non-Blocking (Drop-on-Full para WS)

				// 1. Ruta Persistencia (Bloqueante controlada, la BD no debe perder datos)
				select {
				case d.dbBatchCh <- ev:
				case <-time.After(50 * time.Millisecond):
					log.Printf("[SRE-WARN] Backpressure crítico en BD. Descartando evento máquina %s", ev.MachineID)
				}

				// 2. Ruta HMI / WebSockets (Non-blocking absoluto, es telemetría efímera)
				select {
				case d.wsHubCh <- ev:
				default:
					// Silencio intencional: Si el HMI no lee rápido, descartamos el frame visual.
				}
			}
		}
	})

	// Simulación de Workers acoplados
	d.startDBBatchWorker(ctx)
}

// Publish expone el contrato de entrada para la capa HTTP/UDP.
func (d *EventDispatcher) Publish(ev TelemetryEvent) error {
	// [NUEVO] O(1) Asincrónico
	if d.cep != nil {
		// Fire-and-Forget (No-Block)
		go d.cep.Evaluate(context.Background(), ev)
	}

	select {
	case d.ingestCh <- ev:
		sre.MetricsMESIngested.Add(1) // BIOSENSOR: Éxito
		return nil
	default:
		sre.MetricsMESShedded.Add(1) // BIOSENSOR: Saturación (Drop)
		return fmt.Errorf("dispatcher buffer full: shedding load")
	}
}

// startDBBatchWorker agrupa eventos para salvar PostgreSQL.
func (d *EventDispatcher) startDBBatchWorker(ctx context.Context) {
	d.wg.Go(func() {
		batch := make([]TelemetryEvent, 0, 5000)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				d.flushDB(batch) // Último esfuerzo de guardado al morir
				return
			case ev := <-d.dbBatchCh:
				batch = append(batch, ev)
				if len(batch) >= 5000 {
					d.flushDB(batch)
					clear(batch)
					batch = batch[:0]
				}
			case <-ticker.C:
				if len(batch) > 0 {
					d.flushDB(batch)
					clear(batch)
					batch = batch[:0]
				}
			}
		}
	})
}

func (d *EventDispatcher) flushDB(batch []TelemetryEvent) {
	if len(batch) == 0 {
		return
	}

	err := d.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("sync_outbox"))
		if b == nil {
			return nil
		}

		for _, ev := range batch {
			kb := keyPool.Get().(*[]byte)
			buf := *kb

			// Formatear Timestamp y MachineID (Zero-Alloc)
			binary.BigEndian.PutUint64(buf[0:8], uint64(ev.Timestamp))
			copy(buf[8:], ev.MachineID)

			finalKey := buf[:8+len(ev.MachineID)]

			val, err := json.Marshal(ev)
			if err != nil {
				keyPool.Put(kb)
				continue
			}
			if err := b.Put(finalKey, val); err != nil {
				keyPool.Put(kb)
				return err
			}
			keyPool.Put(kb)
		}
		return nil
	})

	if err != nil {
		log.Printf("[SRE-CRIT] Error fatal volcando telemetría al Outbox: %v", err)
	} else {
		log.Printf("[SRE] %d eventos sellados en Outbox local.", len(batch))
	}
}

// Shutdown garantiza que no dejemos hilos huérfanos.
func (d *EventDispatcher) Shutdown() {
	d.wg.Wait()
}
