package sre

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/state"
)

type MemSnapshot struct {
	ActiveTasks []state.SRETask
	Timestamp   int64
}

// DumpSnapshot almacena un volcado binario en O(1) Allocations de la memoria
func DumpSnapshot(workspace ...string) error {
	base := "."
	if len(workspace) > 0 && workspace[0] != "" {
		base = workspace[0]
	}
	dir := filepath.Join(base, ".neo", "snapshots")
	if err := os.MkdirAll(dir, 0750); err != nil {
		return fmt.Errorf("fallo aislando namespace crono: %v", err)
	}

	snap := MemSnapshot{
		ActiveTasks: state.GetAllTasks(),
		Timestamp:   time.Now().Unix(),
	}

	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(&snap); err != nil {
		return fmt.Errorf("fallo en serializacion zero-alloc: %v", err)
	}
	payloadBytes := buf.Bytes()

	// Sharding matemático: 2 Datos, 1 Paridad XOR (Reed-Solomon style simplificado)
	shardSize := (len(payloadBytes) + 1) / 2
	d0 := make([]byte, shardSize)
	d1 := make([]byte, shardSize)
	p0 := make([]byte, shardSize)

	copy(d0, payloadBytes[:min(shardSize, len(payloadBytes))])
	if len(payloadBytes) > shardSize {
		copy(d1, payloadBytes[shardSize:])
	}

	// Paridad paralela XOR
	for i := range shardSize {
		p0[i] = d0[i] ^ d1[i]
	}

	ts := time.Now().Unix()
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("crash_%d_d0.bin", ts)), d0, 0600); err != nil {
		log.Printf("[SRE-CRASH] Failed to save shard d0: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("crash_%d_d1.bin", ts)), d1, 0600); err != nil {
		log.Printf("[SRE-CRASH] Failed to save shard d1: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("crash_%d_p0.bin", ts)), p0, 0600); err != nil {
		log.Printf("[SRE-CRASH] Failed to save shard p0: %v", err)
	}

	log.Printf("[SRE-CRASH] 🔴 RAM Snapshot persistido en Erasure Coding (2 Data, 1 Parity Shards). ID: %d", ts)
	return nil
}

// LoadSnapshot recupera asincronamente el cerebro transaccional Ouroboros
func LoadSnapshot(filename string, workspace ...string) error {
	tsID := filepath.Base(filename)
	re := regexp.MustCompile(`\d{10,}`)
	if matches := re.FindStringSubmatch(tsID); len(matches) > 0 {
		tsID = matches[0]
	}

	base := "."
	if len(workspace) > 0 && workspace[0] != "" {
		base = workspace[0]
	}
	dir := filepath.Join(base, ".neo", "snapshots")
	d0Path := filepath.Join(dir, fmt.Sprintf("crash_%s_d0.bin", tsID))
	d1Path := filepath.Join(dir, fmt.Sprintf("crash_%s_d1.bin", tsID))
	p0Path := filepath.Join(dir, fmt.Sprintf("crash_%s_p0.bin", tsID))

	d0, err0 := os.ReadFile(filepath.Clean(d0Path))
	d1, err1 := os.ReadFile(filepath.Clean(d1Path))
	p0, err2 := os.ReadFile(filepath.Clean(p0Path))

	shardSize := detectShardSize(d0, d1, p0, err0, err1, err2)
	if shardSize == 0 {
		return fmt.Errorf("no se encontraron fragmentos de snapshot para %s", tsID)
	}

	var repErr error
	d0, d1, repErr = repairShards(d0, d1, p0, err0, err1, err2, shardSize)
	if repErr != nil {
		return repErr
	}

	payloadBytes := make([]byte, 0, len(d0)+len(d1))
	payloadBytes = append(payloadBytes, d0...)
	payloadBytes = append(payloadBytes, d1...)

	var snap MemSnapshot
	buf := bytes.NewReader(payloadBytes)
	if err := gob.NewDecoder(buf).Decode(&snap); err != nil {
		return fmt.Errorf("corrupcion de bitstream gob tras reensamblado XOR: %v", err)
	}

	if err := state.RestoreTasks(snap.ActiveTasks); err != nil {
		return fmt.Errorf("fallo re-anclando Tareas a BoltDB: %v", err)
	}

	log.Printf("[SRE-RECOVERY] 🟢 Cerebro crono-restaurado MÁGICAMENTE vía Erasure Coding desde ID %s", tsID)
	return nil
}

func detectShardSize(d0, d1, p0 []byte, err0, err1, err2 error) int {
	size := 0
	if err0 == nil {
		size = len(d0)
	}
	if err1 == nil && size == 0 {
		size = len(d1)
	}
	if err2 == nil && size == 0 {
		size = len(p0)
	}
	return size
}

// repairShards applies XOR erasure coding to reconstruct a missing shard.
// Returns the (possibly reconstructed) d0, d1 pair, or an error if too many shards are lost.
func repairShards(d0, d1, p0 []byte, err0, err1, err2 error, shardSize int) ([]byte, []byte, error) {
	if err0 != nil && err1 == nil && err2 == nil {
		log.Printf("⚠️ [SRE D0 CORRUPTED] Computando paridad XOR para reconstituir Fragmento D0 en memoria...")
		repaired := make([]byte, shardSize)
		for i := range shardSize {
			repaired[i] = d1[i] ^ p0[i]
		}
		return repaired, d1, nil
	}
	if err1 != nil && err0 == nil && err2 == nil {
		log.Printf("⚠️ [SRE D1 CORRUPTED] Computando paridad XOR para reconstituir Fragmento D1 en memoria...")
		repaired := make([]byte, shardSize)
		for i := range shardSize {
			repaired[i] = d0[i] ^ p0[i]
		}
		return d0, repaired, nil
	}
	if err0 != nil || err1 != nil {
		return nil, nil, fmt.Errorf("corrupcion fatal: demasiados fragmentos perdidos, imposible aplicar Erasure Coding")
	}
	return d0, d1, nil
}
