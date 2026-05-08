package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// pipePair returns the four ends of two unidirectional pipes wired for a
// "server" goroutine to talk to a Client. The Client writes to clientIn and
// reads from clientOut. The "server" reads from serverIn and writes to
// serverOut.
func pipePair() (clientIn io.WriteCloser, clientOut io.ReadCloser, serverIn io.ReadCloser, serverOut io.WriteCloser) {
	cinR, cinW := io.Pipe()
	coutR, coutW := io.Pipe()
	return cinW, coutR, cinR, coutW
}

// fakeServer reads JSON-RPC frames and replies via handler.
func fakeServer(t *testing.T, in io.ReadCloser, out io.WriteCloser, handler func(req map[string]any) any) {
	t.Helper()
	go func() {
		defer in.Close()
		defer out.Close()
		scanner := bufio.NewScanner(in)
		scanner.Buffer(make([]byte, 0, 1<<16), 1<<20)
		enc := json.NewEncoder(out)
		for scanner.Scan() {
			var req map[string]any
			if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
				return
			}
			resp := handler(req)
			if resp == nil {
				continue
			}
			if err := enc.Encode(resp); err != nil {
				return
			}
		}
	}()
}

func TestNamespacedTool_PrefixedName(t *testing.T) {
	cases := []struct {
		prefix, name, want string
	}{
		{"jira", "get_context", "jira_get_context"},
		{"", "lonely", "lonely"},
		{"github", "pr_status", "github_pr_status"},
	}
	for _, tc := range cases {
		got := NamespacedTool{NamespacePrefix: tc.prefix, Tool: Tool{Name: tc.name}}.PrefixedName()
		if got != tc.want {
			t.Errorf("prefix=%q name=%q got %q want %q", tc.prefix, tc.name, got, tc.want)
		}
	}
}

func TestClient_InitializeAndListTools(t *testing.T) {
	clientIn, clientOut, serverIn, serverOut := pipePair()
	fakeServer(t, serverIn, serverOut, func(req map[string]any) any {
		method, _ := req["method"].(string)
		switch method {
		case "initialize":
			return map[string]any{
				"jsonrpc": "2.0",
				"id":      req["id"],
				"result":  map[string]any{"protocolVersion": ProtocolVersion},
			}
		case "tools/list":
			return map[string]any{
				"jsonrpc": "2.0",
				"id":      req["id"],
				"result": map[string]any{
					"tools": []map[string]any{
						{"name": "get_context", "description": "fetch ticket"},
						{"name": "transition", "description": "move ticket"},
					},
				},
			}
		case "notifications/initialized":
			return nil
		}
		return map[string]any{
			"jsonrpc": "2.0", "id": req["id"],
			"error": map[string]any{"code": -32601, "message": "method not found"},
		}
	})

	c := NewClient(clientIn, clientOut)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	tools, err := c.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 2 || tools[0].Name != "get_context" || tools[1].Name != "transition" {
		t.Errorf("unexpected tools: %+v", tools)
	}
	_ = c.Close()
	_ = clientIn.Close()
	_ = clientOut.Close()
}

func TestClient_RPCErrorPropagated(t *testing.T) {
	clientIn, clientOut, serverIn, serverOut := pipePair()
	fakeServer(t, serverIn, serverOut, func(req map[string]any) any {
		return map[string]any{
			"jsonrpc": "2.0", "id": req["id"],
			"error": map[string]any{"code": -32001, "message": "boom"},
		}
	})
	c := NewClient(clientIn, clientOut)
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	_, err := c.ListTools(ctx)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("expected RPC error 'boom', got %v", err)
	}
	_ = clientIn.Close()
	_ = clientOut.Close()
}

func TestClient_ContextTimeout(t *testing.T) {
	clientIn, clientOut, serverIn, serverOut := pipePair()
	// Server NEVER replies — Client must time out via ctx.
	fakeServer(t, serverIn, serverOut, func(req map[string]any) any { return nil })

	c := NewClient(clientIn, clientOut)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := c.ListTools(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected context.DeadlineExceeded, got %v", err)
	}
	_ = clientIn.Close()
	_ = clientOut.Close()
}

func TestClient_ClosedRejectsCalls(t *testing.T) {
	clientIn, clientOut, _, _ := pipePair()
	c := NewClient(clientIn, clientOut)
	_ = c.Close()

	_, err := c.ListTools(context.Background())
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("expected closed error, got %v", err)
	}
	_ = clientIn.Close()
	_ = clientOut.Close()
}

func TestAggregateTools_HappyPath(t *testing.T) {
	// Build two fake plugins, each returning one tool.
	mkConn := func(t *testing.T, name, prefix, toolName string) Connected {
		clientIn, clientOut, serverIn, serverOut := pipePair()
		fakeServer(t, serverIn, serverOut, func(req map[string]any) any {
			if req["method"] == "tools/list" {
				return map[string]any{
					"jsonrpc": "2.0", "id": req["id"],
					"result": map[string]any{"tools": []map[string]any{{"name": toolName}}},
				}
			}
			return nil
		})
		t.Cleanup(func() { _ = clientIn.Close(); _ = clientOut.Close() })
		return Connected{Name: name, NamespacePrefix: prefix, Client: NewClient(clientIn, clientOut)}
	}

	p1 := mkConn(t, "jira", "jira", "get_context")
	p2 := mkConn(t, "github", "gh", "pr_status")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	res := AggregateTools(ctx, []Connected{p1, p2})
	if len(res.Errors) != 0 {
		t.Errorf("unexpected errors: %v", res.Errors)
	}
	if len(res.Tools) != 2 {
		t.Fatalf("tools=%d want 2", len(res.Tools))
	}
	want := map[string]bool{"jira_get_context": false, "gh_pr_status": false}
	for _, nt := range res.Tools {
		want[nt.PrefixedName()] = true
	}
	for k, ok := range want {
		if !ok {
			t.Errorf("missing prefixed tool %q", k)
		}
	}
}

func TestAggregateTools_OnePluginErrors(t *testing.T) {
	clientIn1, clientOut1, serverIn1, serverOut1 := pipePair()
	fakeServer(t, serverIn1, serverOut1, func(req map[string]any) any {
		if req["method"] == "tools/list" {
			return map[string]any{
				"jsonrpc": "2.0", "id": req["id"],
				"result": map[string]any{"tools": []map[string]any{{"name": "ok_tool"}}},
			}
		}
		return nil
	})
	t.Cleanup(func() { _ = clientIn1.Close(); _ = clientOut1.Close() })

	clientIn2, clientOut2, serverIn2, serverOut2 := pipePair()
	fakeServer(t, serverIn2, serverOut2, func(req map[string]any) any {
		return map[string]any{
			"jsonrpc": "2.0", "id": req["id"],
			"error": map[string]any{"code": -32603, "message": "internal"},
		}
	})
	t.Cleanup(func() { _ = clientIn2.Close(); _ = clientOut2.Close() })

	plugins := []Connected{
		{Name: "good", NamespacePrefix: "good", Client: NewClient(clientIn1, clientOut1)},
		{Name: "bad", NamespacePrefix: "bad", Client: NewClient(clientIn2, clientOut2)},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	res := AggregateTools(ctx, plugins)
	if len(res.Tools) != 1 || res.Tools[0].PrefixedName() != "good_ok_tool" {
		t.Errorf("expected [good_ok_tool], got %+v", res.Tools)
	}
	if _, ok := res.Errors["bad"]; !ok {
		t.Error("expected error for plugin 'bad'")
	}
}

func TestClient_CallTool(t *testing.T) {
	clientIn, clientOut, serverIn, serverOut := pipePair()
	fakeServer(t, serverIn, serverOut, func(req map[string]any) any {
		if req["method"] != "tools/call" {
			return nil
		}
		params, _ := req["params"].(map[string]any)
		args, _ := params["arguments"].(map[string]any)
		text, _ := args["text"].(string)
		return map[string]any{
			"jsonrpc": "2.0", "id": req["id"],
			"result": map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": "got:" + text},
				},
			},
		}
	})
	c := NewClient(clientIn, clientOut)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	raw, err := c.CallTool(ctx, "echo", map[string]any{"text": "hi"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !strings.Contains(string(raw), "got:hi") {
		t.Errorf("result=%s want contains 'got:hi'", raw)
	}
	_ = clientIn.Close()
	_ = clientOut.Close()
}

func TestAggregateTools_NilClient(t *testing.T) {
	res := AggregateTools(context.Background(), []Connected{{Name: "ghost", Client: nil}})
	if _, ok := res.Errors["ghost"]; !ok {
		t.Error("nil client should be reported as error")
	}
	if len(res.Tools) != 0 {
		t.Errorf("expected 0 tools, got %d", len(res.Tools))
	}
}
