// Package plugin — mcp.go
// [PILAR-XXIII/123.4] Minimal MCP JSON-RPC client over stdio + namespace
// prefixing + multi-plugin tool aggregator.
//
// Wire format: newline-delimited JSON, per MCP spec for stdio transport.
// One Client owns one (stdin-writer, stdout-reader) pair from a spawned
// plugin process. A single background reader goroutine handles all stdout
// and routes responses to callers by request ID, making CallTool/CallToolWithMeta
// safe for concurrent use from multiple goroutines.
package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// ProtocolVersion advertised in the initialize handshake. Match against any
// well-known MCP version Atlassian/community plugins expect; the field is
// mostly informational for handshake compatibility checks.
const ProtocolVersion = "2024-11-05"

// Tool mirrors the schema element returned by tools/list. Only the fields
// neoanvil cares about today are kept; the rest is preserved as raw JSON via
// InputSchema for forwarding to clients without lossy round-trip.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// NamespacedTool tags a Tool with the plugin it came from, so the Nexus
// aggregator can route a "<prefix>/<name>" call back to the right subprocess.
type NamespacedTool struct {
	PluginName      string
	NamespacePrefix string
	Tool            Tool
}

// PrefixedName returns "<prefix>_<tool>" or just "<tool>" when no prefix is
// configured. The underscore is mandated by the MCP spec (tool names match
// `^[a-zA-Z0-9_-]+$`); a slash separator caused Claude Code to silently
// normalize the name on receive (`deepseek/call` → `deepseek_call`) which
// then failed to match `detectPluginToolCall`'s `<prefix>/` lookup, leaving
// every plugin tools/call falling through to the child neo-mcp registry —
// which doesn't know plugin names. Captured by ÉPICA 152 (PILAR XXIX).
func (n NamespacedTool) PrefixedName() string {
	if n.NamespacePrefix == "" {
		return n.Tool.Name
	}
	return n.NamespacePrefix + "_" + n.Tool.Name
}

// Connected pairs a live Client with the metadata needed by the aggregator
// to namespace its tools. Callers (Nexus) build this from a spawned
// PluginProcess + NewClient.
type Connected struct {
	Name              string
	NamespacePrefix   string
	ToolName          string   // MCP tool name exposed by this plugin (e.g. "jira", "call")
	Client            *Client
	AllowedWorkspaces []string // [P-WSACL] copied from PluginSpec at handshake. Empty = all workspaces.
}

// AggregateResult holds the merged tool list plus per-plugin errors. One
// plugin failing does not block the others — the caller decides whether
// to escalate.
type AggregateResult struct {
	Tools  []NamespacedTool
	Errors map[string]error
}

// AggregateTools queries each connected plugin for its tools/list and
// flattens the result into namespaced tools. Errors are reported per-plugin.
func AggregateTools(ctx context.Context, plugins []Connected) AggregateResult {
	res := AggregateResult{Errors: make(map[string]error)}
	for _, p := range plugins {
		if p.Client == nil {
			res.Errors[p.Name] = errors.New("nil client")
			continue
		}
		tools, err := p.Client.ListTools(ctx)
		if err != nil {
			res.Errors[p.Name] = err
			continue
		}
		for _, t := range tools {
			res.Tools = append(res.Tools, NamespacedTool{
				PluginName:      p.Name,
				NamespacePrefix: p.NamespacePrefix,
				Tool:            t,
			})
		}
	}
	return res
}

// rpcRequest / rpcResponse / rpcError are the JSON-RPC 2.0 envelopes.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// readResult is the outcome delivered by the background reader to a waiter.
type readResult struct {
	resp *rpcResponse
	err  error
}

// Client is a concurrent-safe MCP JSON-RPC stdio client. Multiple goroutines
// may call CallTool / CallToolWithMeta simultaneously. A single background
// reader goroutine routes responses to callers by request ID; only the
// encode step is serialized.
type Client struct {
	sendMu sync.Mutex   // serializes JSON-RPC frame writes to stdin
	enc    *json.Encoder

	waitMu  sync.Mutex
	waiters map[int64]chan readResult

	nextID atomic.Int64
	closed atomic.Bool

	// dead is closed by the background reader goroutine on exit (error or EOF).
	dead    chan struct{}
	deadMu  sync.Mutex
	deadErr error

	// ServerProtocol/Name/Version captured from the initialize handshake response.
	ServerProtocol string
	ServerName     string
	ServerVersion  string
}

// NewClient wraps the stdin (writer) and stdout (reader) pipes of a spawned
// plugin process and starts the background response reader. The Client does
// NOT take ownership of the pipes — close them in the caller after Close().
func NewClient(stdin io.Writer, stdout io.Reader) *Client {
	c := &Client{
		enc:     json.NewEncoder(stdin),
		waiters: make(map[int64]chan readResult),
		dead:    make(chan struct{}),
	}
	go c.readerLoop(bufio.NewReaderSize(stdout, 64*1024))
	return c
}

// readerLoop is the single background goroutine that reads all plugin stdout.
// It routes each response to the waiter registered for that request ID.
// On any exit (including panic), it delivers the error to all pending waiters
// and closes c.dead to unblock any call() blocked in select.
func (c *Client) readerLoop(rd *bufio.Reader) {
	var rErr error
	defer func() {
		if r := recover(); r != nil {
			rErr = fmt.Errorf("reader panic: %v", r)
		}
		if rErr == nil {
			rErr = io.EOF
		}
		c.deadMu.Lock()
		c.deadErr = rErr
		c.deadMu.Unlock()
		// Wake all pending callers before closing dead so the delivery via
		// ch fires before the dead channel signals.
		c.waitMu.Lock()
		for id, ch := range c.waiters {
			select {
			case ch <- readResult{nil, rErr}:
			default:
			}
			delete(c.waiters, id)
		}
		c.waitMu.Unlock()
		close(c.dead)
	}()

	for {
		line, err := rd.ReadBytes('\n')
		if err != nil {
			rErr = err
			return
		}
		var resp rpcResponse
		if jsonErr := json.Unmarshal(line, &resp); jsonErr != nil {
			// Malformed frame: skip; do not kill the connection over one bad line.
			continue
		}
		c.waitMu.Lock()
		ch, ok := c.waiters[resp.ID]
		if ok {
			delete(c.waiters, resp.ID)
		}
		c.waitMu.Unlock()
		if ok {
			select {
			case ch <- readResult{&resp, nil}:
			default:
				// Waiter already gone (context cancelled between register and receive).
			}
		}
	}
}

// Initialize performs the MCP handshake: sends `initialize`, waits for the
// response, then fires the `notifications/initialized` notification.
//
// [P3] Captures serverInfo + protocolVersion from the response. Rejects
// plugins that declare a protocol version newer than ProtocolVersion
// (forward-incompatible). Older versions produce a log warning but proceed.
func (c *Client) Initialize(ctx context.Context) error {
	params, err := json.Marshal(map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "neo-nexus", "version": "0.1"},
	})
	if err != nil {
		return fmt.Errorf("marshal init params: %w", err)
	}
	result, err := c.call(ctx, "initialize", params)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	// Parse server protocol version + info. Fail-open: if the response is
	// missing these fields (non-compliant plugin) we log but continue.
	var info struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	if jsonErr := json.Unmarshal(result, &info); jsonErr == nil {
		c.ServerProtocol = info.ProtocolVersion
		c.ServerName = info.ServerInfo.Name
		c.ServerVersion = info.ServerInfo.Version
		// Date strings compare lexicographically: "2025-01-01" > "2024-11-05" ✓
		if info.ProtocolVersion > ProtocolVersion {
			return fmt.Errorf("protocol mismatch: plugin requires %s, nexus supports %s — upgrade nexus or pin plugin version",
				info.ProtocolVersion, ProtocolVersion)
		}
	}
	if err := c.notify("notifications/initialized", nil); err != nil {
		return fmt.Errorf("notify initialized: %w", err)
	}
	return nil
}

// ListTools issues `tools/list` and returns the parsed tool slice.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	raw, err := c.call(ctx, "tools/list", json.RawMessage(`{}`))
	if err != nil {
		return nil, err
	}
	var resp struct {
		Tools []Tool `json:"tools"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal tools: %w", err)
	}
	return resp.Tools, nil
}

// CallTool issues `tools/call` for the given tool name + arguments. Returns
// the raw `result` object — caller decodes the MCP content shape (typically
// `{"content": [{"type": "text", "text": "..."}]}`).
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (json.RawMessage, error) {
	params, err := json.Marshal(map[string]any{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal call params: %w", err)
	}
	return c.call(ctx, "tools/call", params)
}

// CallToolWithMeta issues `tools/call` injecting _meta at the params level.
// The _meta field is reserved by the MCP spec for implementation metadata
// (idempotency keys, trace IDs) that plugins may inspect but must not treat
// as tool arguments — it lives outside `arguments` to avoid schema conflicts. [P2+P4]
func (c *Client) CallToolWithMeta(ctx context.Context, name string, args map[string]any, meta map[string]any) (json.RawMessage, error) {
	params, err := json.Marshal(map[string]any{
		"name":      name,
		"arguments": args,
		"_meta":     meta,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal call params: %w", err)
	}
	return c.call(ctx, "tools/call", params)
}

// Close marks the client as closed. Subsequent calls return an error immediately.
// The underlying pipes are owned by the caller; killing the subprocess will
// cause the background reader to exit and unblock any calls waiting for responses.
func (c *Client) Close() error {
	c.closed.Store(true)
	return nil
}

// call sends a JSON-RPC request and waits for the matching response. It is
// safe for concurrent use from multiple goroutines.
func (c *Client) call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	if c.closed.Load() {
		return nil, errors.New("client closed")
	}

	id := c.nextID.Add(1)
	ch := make(chan readResult, 1)

	// Register the waiter BEFORE sending to ensure we never miss the response.
	c.waitMu.Lock()
	c.waiters[id] = ch
	c.waitMu.Unlock()
	defer func() {
		c.waitMu.Lock()
		delete(c.waiters, id)
		c.waitMu.Unlock()
	}()

	// Serialize only the encode step — the wait below is lock-free.
	c.sendMu.Lock()
	encErr := c.enc.Encode(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	c.sendMu.Unlock()
	if encErr != nil {
		return nil, fmt.Errorf("encode: %w", encErr)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		return validateResponse(r.resp, r.err, id)
	case <-c.dead:
		c.deadMu.Lock()
		dErr := c.deadErr
		c.deadMu.Unlock()
		if dErr == nil {
			dErr = errors.New("plugin connection closed")
		}
		return nil, dErr
	}
}

// validateResponse maps a (response, err, expectedID) triple into the
// final return shape, kept separate so call() stays under the CC cap.
func validateResponse(resp *rpcResponse, readErr error, expectID int64) (json.RawMessage, error) {
	if readErr != nil {
		return nil, readErr
	}
	if resp.ID != expectID {
		return nil, fmt.Errorf("response id=%d want %d", resp.ID, expectID)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("rpc error: %s (code %d)", resp.Error.Message, resp.Error.Code)
	}
	return resp.Result, nil
}

// notify sends a JSON-RPC notification (no ID, no response expected).
func (c *Client) notify(method string, params json.RawMessage) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return c.enc.Encode(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
}
