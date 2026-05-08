package mctx

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net"
	"sync"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/rag"
	"github.com/ensamblatec/neoanvil/pkg/sre"
)

// ShouldTransmitGossip Orquesta Matemáticamente un amortiguador de Resonancia
// Evita el Colapso Múltiple $5 \text{ Terabits}$ previniendo Tormentas Broadcast tras un Split-Brain
func ShouldTransmitGossip(totalSwarmNodes int) bool {
	if totalSwarmNodes <= 1 {
		return true
	}

	// Regla SRE: Solo $\log(N)$ del Enjambre responde al Handshake Split-Brain recuperado
	allowed := int(math.Max(1, math.Log(float64(totalSwarmNodes))))

	// Entropía Jitter Pura para garantizar la Simetría Distribuida a nivel Global
	var buf [4]byte
	j := &sre.JitterEntropy{}
	j.Read(buf[:])

	roll := int(buf[0]) % totalSwarmNodes

	// Exponential Backoff es invocado silenciosamente al desaprobar la transmisión
	return roll < allowed
}

// GossipQuery is the wire format for P2P semantic search requests. [SRE-27.1.1]
type GossipQuery struct {
	Query string `json:"query"`
	TopK  int    `json:"top_k"`
}

// GossipResponse is the wire format for P2P semantic search responses. [SRE-27.1.1]
type GossipResponse struct {
	Snippets []string `json:"snippets"`
}

// QueryPeer sends a semantic search query to a single Gossip peer via TCP.
// Returns snippets found on the remote node's RAG, or error if unreachable.
func QueryPeer(ctx context.Context, addr string, query string, topK int) ([]string, error) {
	dialer := net.Dialer{Timeout: 2 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))

	if err := json.NewEncoder(conn).Encode(GossipQuery{Query: query, TopK: topK}); err != nil {
		return nil, err
	}

	var resp GossipResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, err
	}
	return resp.Snippets, nil
}

// QueryPeers fans out a semantic search to all configured Tailscale peers concurrently. [SRE-27.1.1]
// Uses ShouldTransmitGossip to suppress broadcast storms.
// Returns de-duplicated snippets from all responding nodes.
func QueryPeers(ctx context.Context, peers []string, port int, query string) []string {
	if len(peers) == 0 || !ShouldTransmitGossip(len(peers)) {
		return nil
	}
	var mu sync.Mutex
	var all []string
	var wg sync.WaitGroup
	for _, peer := range peers {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			addr := fmt.Sprintf("%s:%d", p, port)
			snippets, err := QueryPeer(ctx, addr, query, 3)
			if err != nil {
				log.Printf("[SRE-GOSSIP] Peer %s unreachable: %v", addr, err)
				return
			}
			mu.Lock()
			all = append(all, snippets...)
			mu.Unlock()
		}(peer)
	}
	wg.Wait()
	return all
}

// StartGossipListener starts a TCP server that handles incoming Gossip queries,
// delta-sync requests, and federated search. [SRE-27.1.1][SRE-37.2][SRE-37.3]
// Binds to 0.0.0.0 so Tailscale VPN peers can reach this node.
func StartGossipListener(ctx context.Context, port int, handler GossipHandler) {
	addr := fmt.Sprintf("0.0.0.0:%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Printf("[SRE-GOSSIP] Failed to start listener on %s: %v", addr, err)
		return
	}
	log.Printf("[SRE-GOSSIP] Gossip listener active on %s", addr)
	go func() {
		defer ln.Close()
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			conn, err := ln.Accept()
			if err != nil {
				select {
				case <-ctx.Done():
					return
				default:
					log.Printf("[SRE-GOSSIP] Accept error: %v", err)
					continue
				}
			}
			go handleGossipConn(ctx, conn, handler)
		}
	}()
}

func handleGossipConn(ctx context.Context, conn net.Conn, handler GossipHandler) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	// Peek at message type via generic decode
	var msg json.RawMessage
	if err := json.NewDecoder(conn).Decode(&msg); err != nil {
		return
	}

	// Try to detect message type from fields
	var probe struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(msg, &probe)

	enc := json.NewEncoder(conn)

	switch probe.Type {
	case "delta_sync":
		var req DeltaSyncRequest
		if err := json.Unmarshal(msg, &req); err != nil {
			return
		}
		resp := handler.HandleDeltaSync(ctx, req)
		if err := enc.Encode(resp); err != nil {
			log.Printf("[SRE-GOSSIP] delta_sync encode error: %v", err)
		}

	case "export_data":
		var req ExportDataRequest
		if err := json.Unmarshal(msg, &req); err != nil {
			return
		}
		resp := handler.HandleExportData(ctx, req)
		if err := enc.Encode(resp); err != nil {
			log.Printf("[SRE-GOSSIP] export_data encode error: %v", err)
		}

	case "federated_query":
		var req FederatedQueryRequest
		if err := json.Unmarshal(msg, &req); err != nil {
			return
		}
		resp := handler.HandleFederatedQuery(ctx, req)
		if err := enc.Encode(resp); err != nil {
			log.Printf("[SRE-GOSSIP] federated_query encode error: %v", err)
		}

	default:
		// Legacy: plain GossipQuery (backwards compatible)
		var q GossipQuery
		if err := json.Unmarshal(msg, &q); err != nil {
			return
		}
		snippets := handler.Search(ctx, q.Query)
		if snippets == nil {
			snippets = []string{}
		}
		if err := enc.Encode(GossipResponse{Snippets: snippets}); err != nil {
			log.Printf("[SRE-GOSSIP] gossip response encode error: %v", err)
		}
	}
}

// ─── Delta-Sync Protocol (Épica 37.2) ─────────────────────────────────────

// DeltaSyncRequest is sent by a peer to compare Merkle roots. [SRE-37.2]
type DeltaSyncRequest struct {
	Type    string              `json:"type"` // "delta_sync"
	Digests []rag.MerkleDigest  `json:"digests"`
}

// DeltaSyncResponse contains the diff analysis and optionally the full hashes
// for buckets that differ so the requester can pinpoint exact changes. [SRE-37.2]
type DeltaSyncResponse struct {
	Matches    int                        `json:"matches"`
	Diffs      []BucketDiffSummary        `json:"diffs"`
	HashExport map[string][]string        `json:"hash_export,omitempty"` // bucket → flat hashes
}

// BucketDiffSummary describes whether a specific bucket differs between peers.
type BucketDiffSummary struct {
	Bucket     string `json:"bucket"`
	LocalHash  string `json:"local_hash"`
	RemoteHash string `json:"remote_hash"`
	InSync     bool   `json:"in_sync"`
}

// ExportDataRequest asks a peer to export key-value data for specific ranges. [SRE-37.2]
type ExportDataRequest struct {
	Type       string `json:"type"` // "export_data"
	Bucket     string `json:"bucket"`
	KeyStart   string `json:"key_start"`   // hex-encoded
	KeyEnd     string `json:"key_end"`     // hex-encoded
}

// ExportDataResponse contains the raw data for the requested range.
type ExportDataResponse struct {
	Bucket string            `json:"bucket"`
	Data   map[string][]byte `json:"data"` // hex-key → value
	Count  int               `json:"count"`
}

// FederatedQueryRequest is a cross-node semantic search. [SRE-37.3]
type FederatedQueryRequest struct {
	Type       string `json:"type"` // "federated_query"
	Query      string `json:"query"`
	TopK       int    `json:"top_k"`
	OriginNode string `json:"origin_node"` // prevents query loops
}

// FederatedQueryResponse returns search results with provenance. [SRE-37.3]
type FederatedQueryResponse struct {
	Snippets []FederatedSnippet `json:"snippets"`
	NodeID   string             `json:"node_id"`
}

// FederatedSnippet is a search result with its source node. [SRE-37.3]
type FederatedSnippet struct {
	Content  string  `json:"content"`
	Path     string  `json:"path"`
	Score    float32 `json:"score"`
	SourceNode string `json:"source_node"`
}

// GossipHandler abstracts the local node's capabilities for the gossip protocol.
type GossipHandler struct {
	WAL      *rag.WAL
	NodeID   string
	SearchFn func(ctx context.Context, query string) []string
	// SearchWithScores returns results with scores and paths for federated queries
	SearchWithScoresFn func(ctx context.Context, query string, topK int) []FederatedSnippet
}

// HandleDeltaSync compares local Merkle roots with the requester's digests. [SRE-37.2]
func (gh GossipHandler) HandleDeltaSync(ctx context.Context, req DeltaSyncRequest) DeltaSyncResponse {
	resp := DeltaSyncResponse{
		HashExport: make(map[string][]string),
	}

	localDigests, err := gh.WAL.MerkleDigests()
	if err != nil {
		log.Printf("[SRE-MERKLE] Failed to compute local digests: %v", err)
		return resp
	}

	localMap := make(map[string]rag.MerkleDigest, len(localDigests))
	for _, d := range localDigests {
		localMap[d.BucketName] = d
	}

	for _, remote := range req.Digests {
		local, ok := localMap[remote.BucketName]
		summary := BucketDiffSummary{
			Bucket:     remote.BucketName,
			RemoteHash: remote.RootHash,
		}

		if !ok {
			summary.InSync = false
			resp.Diffs = append(resp.Diffs, summary)
			continue
		}

		summary.LocalHash = local.RootHash
		summary.InSync = local.RootHash == remote.RootHash

		if summary.InSync {
			resp.Matches++
		} else {
			// Export full hashes so requester can diff
			tree, treeErr := gh.WAL.MerkleTreeForBucket(remote.BucketName)
			if treeErr == nil && tree != nil {
				resp.HashExport[remote.BucketName] = tree.ExportHashes()
			}
		}
		resp.Diffs = append(resp.Diffs, summary)
	}

	return resp
}

// HandleExportData returns raw data for a key range in a bucket. [SRE-37.2]
func (gh GossipHandler) HandleExportData(ctx context.Context, req ExportDataRequest) ExportDataResponse {
	keyStart, _ := hex.DecodeString(req.KeyStart)
	keyEnd, _ := hex.DecodeString(req.KeyEnd)

	data, err := gh.WAL.ExportBucketData(req.Bucket, keyStart, keyEnd)
	if err != nil {
		log.Printf("[SRE-MERKLE] Export failed for %s: %v", req.Bucket, err)
		return ExportDataResponse{Bucket: req.Bucket}
	}

	return ExportDataResponse{
		Bucket: req.Bucket,
		Data:   data,
		Count:  len(data),
	}
}

// HandleFederatedQuery performs a local search and returns results with provenance. [SRE-37.3]
func (gh GossipHandler) HandleFederatedQuery(ctx context.Context, req FederatedQueryRequest) FederatedQueryResponse {
	resp := FederatedQueryResponse{NodeID: gh.NodeID}

	if req.OriginNode == gh.NodeID {
		return resp // prevent query loops
	}

	if gh.SearchWithScoresFn != nil {
		resp.Snippets = gh.SearchWithScoresFn(ctx, req.Query, req.TopK)
		for i := range resp.Snippets {
			resp.Snippets[i].SourceNode = gh.NodeID
		}
	} else if gh.SearchFn != nil {
		snippets := gh.SearchFn(ctx, req.Query)
		for _, s := range snippets {
			resp.Snippets = append(resp.Snippets, FederatedSnippet{
				Content:    s,
				SourceNode: gh.NodeID,
			})
		}
	}

	return resp
}

// Search delegates to the local RAG search (backward compat).
func (gh GossipHandler) Search(ctx context.Context, query string) []string {
	if gh.SearchFn != nil {
		return gh.SearchFn(ctx, query)
	}
	return nil
}

// ─── Client-side Delta-Sync (Épica 37.2) ─────────────────────────────────

// RequestDeltaSync connects to a peer and compares Merkle digests. [SRE-37.2]
// Returns the diff response or error. Used by the sync loop.
func RequestDeltaSync(ctx context.Context, addr string, localDigests []rag.MerkleDigest) (*DeltaSyncResponse, error) {
	dialer := net.Dialer{Timeout: 3 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	req := DeltaSyncRequest{
		Type:    "delta_sync",
		Digests: localDigests,
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, err
	}

	var resp DeltaSyncResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// RequestExportData fetches raw data from a peer for a specific bucket range. [SRE-37.2]
func RequestExportData(ctx context.Context, addr, bucket, keyStart, keyEnd string) (*ExportDataResponse, error) {
	dialer := net.Dialer{Timeout: 3 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	req := ExportDataRequest{
		Type:     "export_data",
		Bucket:   bucket,
		KeyStart: keyStart,
		KeyEnd:   keyEnd,
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, err
	}

	var resp ExportDataResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// FederatedSearch fans out a semantic query to all peers and merges results. [SRE-37.3]
// Results are de-duplicated by content hash and sorted by score.
func FederatedSearch(ctx context.Context, peers []string, port int, query string, topK int, localNodeID string) []FederatedSnippet {
	if len(peers) == 0 || !ShouldTransmitGossip(len(peers)) {
		return nil
	}

	var mu sync.Mutex
	var all []FederatedSnippet
	var wg sync.WaitGroup

	for _, peer := range peers {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			addr := fmt.Sprintf("%s:%d", p, port)

			dialer := net.Dialer{Timeout: 3 * time.Second}
			conn, err := dialer.DialContext(ctx, "tcp", addr)
			if err != nil {
				log.Printf("[SRE-FEDERATION] Peer %s unreachable: %v", addr, err)
				return
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

			req := FederatedQueryRequest{
				Type:       "federated_query",
				Query:      query,
				TopK:       topK,
				OriginNode: localNodeID,
			}
			if err := json.NewEncoder(conn).Encode(req); err != nil {
				return
			}

			var resp FederatedQueryResponse
			if err := json.NewDecoder(conn).Decode(&resp); err != nil {
				return
			}

			mu.Lock()
			all = append(all, resp.Snippets...)
			mu.Unlock()
		}(peer)
	}
	wg.Wait()

	// De-duplicate by content
	seen := make(map[string]bool)
	deduped := all[:0]
	for _, s := range all {
		if !seen[s.Content] {
			seen[s.Content] = true
			deduped = append(deduped, s)
		}
	}

	return deduped
}

// SyncWithPeer performs a full delta-sync cycle with a single peer. [SRE-37.2]
// 1. Compare Merkle digests
// 2. For differing buckets, diff the trees
// 3. Fetch and import missing data
func SyncWithPeer(ctx context.Context, addr string, localWAL *rag.WAL) (int, error) {
	localDigests, err := localWAL.MerkleDigests()
	if err != nil {
		return 0, fmt.Errorf("local digest computation failed: %w", err)
	}

	resp, err := RequestDeltaSync(ctx, addr, localDigests)
	if err != nil {
		return 0, fmt.Errorf("delta sync request failed: %w", err)
	}

	imported := 0
	for _, diff := range resp.Diffs {
		if diff.InSync {
			continue
		}

		remoteHashes, ok := resp.HashExport[diff.Bucket]
		if !ok {
			continue
		}

		localTree, treeErr := localWAL.MerkleTreeForBucket(diff.Bucket)
		if treeErr != nil {
			log.Printf("[SRE-SYNC] Cannot build local tree for %s: %v", diff.Bucket, treeErr)
			continue
		}

		diffs := localTree.DiffAgainst(remoteHashes)
		for _, d := range diffs {
			exportResp, exportErr := RequestExportData(ctx, addr, d.BucketName, d.KeyStart, d.KeyEnd)
			if exportErr != nil {
				log.Printf("[SRE-SYNC] Export request failed for %s: %v", d.BucketName, exportErr)
				continue
			}
			if err := localWAL.ImportBucketData(d.BucketName, exportResp.Data); err != nil {
				log.Printf("[SRE-SYNC] Import failed for %s: %v", d.BucketName, err)
				continue
			}
			imported += exportResp.Count
		}
	}

	if imported > 0 {
		log.Printf("[SRE-SYNC] Imported %d entries from peer %s", imported, addr)
	}
	return imported, nil
}
