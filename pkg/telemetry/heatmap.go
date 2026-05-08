package telemetry

import (
	"encoding/binary"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"go.etcd.io/bbolt"
)

var (
	heatDB   *bbolt.DB
	heatMu   sync.Mutex
	heatPath string
)

const (
	heatmapBucket       = "AST_HEATMAP"
	heatmapBypassBucket = "AST_HEATMAP_BYPASS" // mutations committed via NEO_CERTIFY_BYPASS [Épica 159]
)

func InitHeatmap(workspace string) error {
	heatPath = filepath.Join(workspace, ".neo/db", "telemetry_heatmap.db")
	return tryInit()
}

func tryInit() error {
	heatMu.Lock()
	defer heatMu.Unlock()

	if heatDB != nil {
		return nil
	}
	if heatPath == "" {
		return fmt.Errorf("heatmap path not set")
	}

	db, err := bbolt.Open(heatPath, 0600, &bbolt.Options{Timeout: 200 * time.Millisecond})
	if err != nil {
		return err
	}

	err = db.Update(func(tx *bbolt.Tx) error {
		if _, e := tx.CreateBucketIfNotExists([]byte(heatmapBucket)); e != nil {
			return e
		}
		_, e := tx.CreateBucketIfNotExists([]byte(heatmapBypassBucket))
		return e
	})

	if err != nil {
		db.Close()
		return err
	}

	heatDB = db
	return nil
}

func RecordMutation(filepath string) error {
	if heatDB == nil {
		if err := tryInit(); err != nil {
			return fmt.Errorf("heatmap db offline: %v", err)
		}
	}

	return heatDB.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(heatmapBucket))
		if b == nil {
			return fmt.Errorf("bucket not found")
		}

		key := []byte(filepath)
		val := b.Get(key)
		var mutations uint64 = 0

		if val != nil {
			mutations = binary.BigEndian.Uint64(val)
		}

		mutations++

		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], mutations)
		return b.Put(key, buf[:])
	})
}

type Hotspot struct {
	File      string
	Mutations uint64 // certified mutations
	Bypassed  uint64 // mutations committed via NEO_CERTIFY_BYPASS [159.B]
}

// RecordBypassMutation records a bypass mutation in the dedicated bypass bucket. [Épica 159.A]
func RecordBypassMutation(fp string) error {
	if heatDB == nil {
		if err := tryInit(); err != nil {
			return fmt.Errorf("heatmap db offline: %v", err)
		}
	}
	return heatDB.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(heatmapBypassBucket))
		if b == nil {
			return fmt.Errorf("bypass bucket not found")
		}
		key := []byte(fp)
		val := b.Get(key)
		var count uint64
		if val != nil {
			count = binary.BigEndian.Uint64(val)
		}
		count++
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], count)
		return b.Put(key, buf[:])
	})
}

func GetTopHotspots(limit int) ([]Hotspot, error) {
	if heatDB == nil {
		if err := tryInit(); err != nil {
			return nil, fmt.Errorf("heatmap db offline: %v", err)
		}
	}

	// Build certified map first, then enrich with bypass counts. [159.C]
	hotspotMap := make(map[string]*Hotspot)

	err := heatDB.View(func(tx *bbolt.Tx) error {
		if b := tx.Bucket([]byte(heatmapBucket)); b != nil {
			if forErr := b.ForEach(func(k, v []byte) error {
				file := string(k)
				hotspotMap[file] = &Hotspot{File: file, Mutations: binary.BigEndian.Uint64(v)}
				return nil
			}); forErr != nil {
				return forErr
			}
		}
		if bb := tx.Bucket([]byte(heatmapBypassBucket)); bb != nil {
			return bb.ForEach(func(k, v []byte) error {
				file := string(k)
				if hs, ok := hotspotMap[file]; ok {
					hs.Bypassed = binary.BigEndian.Uint64(v)
				} else {
					hotspotMap[file] = &Hotspot{File: file, Bypassed: binary.BigEndian.Uint64(v)}
				}
				return nil
			})
		}
		return nil
	})

	var results []Hotspot
	for _, hs := range hotspotMap {
		results = append(results, *hs)
	}

	if err != nil {
		return nil, err
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Mutations > results[j].Mutations
	})

	if len(results) > limit {
		results = results[:limit]
	}

	return results, nil
}

func CloseHeatmap() {
	if heatDB != nil {
		heatDB.Close()
	}
}
