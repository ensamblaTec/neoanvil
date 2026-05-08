package edgesync

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"go.etcd.io/bbolt"
	"github.com/ensamblatec/neoanvil/pkg/sre"
	"github.com/ensamblatec/neoanvil/pkg/telemetry"
)

// Forwarder se encarga de expulsar la telemetría MES del Edge Node
// hacia el Cloud/ERP de forma garantizada y asimétrica.
type Forwarder struct {
	db       *bbolt.DB
	cloudURL string
}

func NewForwarder(db *bbolt.DB, cloudURL string) *Forwarder {
	return &Forwarder{db: db, cloudURL: cloudURL}
}

func (f *Forwarder) fetchNextPayload() (key []byte, val []byte, err error) {
	err = f.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("sync_outbox"))
		if b == nil {
			return nil
		}
		c := b.Cursor()
		k, v := c.First()
		if k != nil {
			key = append([]byte{}, k...)
			val = append([]byte{}, v...)
		}
		return nil
	})
	return key, val, err
}

func (f *Forwarder) sendToCloud(ctx context.Context, client *http.Client, val []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.cloudURL, bytes.NewReader(val))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return http.ErrServerClosed // Simulate error for retry logic
	}
	return nil
}

func (f *Forwarder) deletePayload(key []byte) error {
	return f.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("sync_outbox"))
		if b != nil {
			return b.Delete(key)
		}
		return nil
	})
}

func (f *Forwarder) Start(ctx context.Context) {
	log.Printf("[SRE] Forwarder iniciado contra ERP %s", f.cloudURL)
	backoff := 1 * time.Second
	maxBackoff := 60 * time.Second
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	client := sre.SafeHTTPClient()

	for {
		select {
		case <-ctx.Done():
			log.Println("[SRE] Apagando el Forwarder")
			return
		case <-ticker.C:
			key, val, err := f.fetchNextPayload()
			if err != nil || len(key) == 0 {
				continue
			}

			if err := f.sendToCloud(ctx, client, val); err != nil {
				log.Printf("[SRE-WARN] Fallo ERP nube (Endpoint: %s). Reintentando en %v", f.cloudURL, backoff)
				sre.MetricsSyncFailed.Add(1)
				telemetry.EmitEvent("TOPOLOGÍA", fmt.Sprintf("[BGP] Cloud ERP unreachable: %v | Retrying...", backoff))

				// [sre-patch] Asynchronous Backoff. Non-Blocking SRE
				ticker.Reset(backoff)
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
				continue
			}

			backoff = 1 * time.Second
			ticker.Reset(2 * time.Second)

			if err := f.deletePayload(key); err != nil {
				sre.MetricsSyncFailed.Add(1)
				continue
			}
			sre.MetricsSyncSuccess.Add(1)
			telemetry.EmitEvent("TOPOLOGÍA", "[BGP] Cloud ERP in Sync (0ms)")
			log.Printf("[SRE-HIT] Telemetría evacuadas a Nube correctamente. (Key: %s)", string(key))
		}
	}
}
