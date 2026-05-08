package rag

import (
	"math"
	"os"
	"sort"
	"time"
)

type DocumentScore struct {
	DocID   uint64
	Rank    int
	Score   float32
	AgeDays float64
}

func FuseResults(lexical, vectorial []DocumentScore, timeDecayLambda float64, wal *WAL) []DocumentScore {
	fusionMap := make(map[uint64]float32, len(lexical)+len(vectorial))
	ages := make(map[uint64]float64, len(lexical)+len(vectorial))

	const k = 60.0

	for _, doc := range lexical {
		fusionMap[doc.DocID] += 1.0 / (k + float32(doc.Rank))
		ages[doc.DocID] = doc.AgeDays
	}

	for _, doc := range vectorial {
		fusionMap[doc.DocID] += 1.0 / (k + float32(doc.Rank))
		ages[doc.DocID] = doc.AgeDays
	}

	result := make([]DocumentScore, 0, len(fusionMap))
	for id, rrfScore := range fusionMap {
		timeWeight := float32(math.Exp(-timeDecayLambda * ages[id]))
		score := rrfScore * timeWeight

		if wal != nil {
			path, _, _, err := wal.GetDocMeta(id)
			if err == nil {
				fileInfo, statErr := os.Stat(path)
				if statErr == nil {
					daysOld := time.Since(fileInfo.ModTime()).Hours() / 24
					penalty := 1.0
					if daysOld > 180 {
						penalty = 0.5
					} else if daysOld > 30 {
						penalty = 0.7
					} else if daysOld > 7 {
						penalty = 0.9
					}
					score = score * float32(penalty)
				}
			}
		}

		result = append(result, DocumentScore{
			DocID:   id,
			Score:   score,
			AgeDays: ages[id],
		})
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Score > result[j].Score
	})

	return result
}
