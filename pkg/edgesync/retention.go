package edgesync

import (
	"context"
	"log"
	"time"

	"go.etcd.io/bbolt"
	"github.com/ensamblatec/neoanvil/pkg/sre"
)

// StartRetentionSweeper protege el Edge Node contra el agotamiento de disco
// cuando la conexión con el Cloud ERP se pierde por tiempos prolongados.
func StartRetentionSweeper(ctx context.Context, db *bbolt.DB, maxThreshold int) {
	// Evaluamos la presión de disco cada minuto.
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	log.Printf("[SRE] Retention Sweeper activado. Límite del Outbox: %d registros.", maxThreshold)

	for {
		select {
		case <-ctx.Done():
			log.Println("[SRE] Apagando Retention Sweeper")
			return
		case <-ticker.C:
			enforceRetentionPolicy(db, maxThreshold)
		}
	}
}

func enforceRetentionPolicy(db *bbolt.DB, maxThreshold int) {
	err := db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("sync_outbox"))
		if b == nil {
			return nil
		}

		// Lectura rápida O(1) de la cantidad de llaves en el bucket
		count := b.Stats().KeyN
		if count <= maxThreshold {
			return nil // Disco bajo control
		}

		// Shedding Táctico: Si superamos el umbral, sacrificamos el 20% más antiguo
		// para dar respiro al disco y evitar purgas constantes.
		toDelete := (count - maxThreshold) + int(float64(maxThreshold)*0.2)

		log.Printf("[SRE-CRIT] Presión de disco extrema (Outbox: %d). ERP Offline. Descartando los %d eventos más antiguos...", count, toDelete)

		c := b.Cursor()
		deleted := 0

		for k, _ := c.First(); k != nil && deleted < toDelete; k, _ = c.Next() {
			if err := c.Delete(); err != nil {
				return err
			}
			deleted++
			// BIOSENSOR: Registrar la pérdida de datos forzada para auditoría posterior
			sre.MetricsMESShedded.Add(1)
		}

		log.Printf("[SRE-RECOVERY] Purga completada. %d eventos eliminados. Nodo estabilizado.", deleted)
		return nil
	})

	if err != nil {
		log.Printf("[SRE-FATAL] Fallo en el motor de retención al intentar liberar disco: %v", err)
	}
}
