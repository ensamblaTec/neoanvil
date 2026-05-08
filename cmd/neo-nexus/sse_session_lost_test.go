package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRespondSessionLost_StructuredJSON verifies the body of a session-lost
// response carries the JSON-RPC envelope with the new fallback fields. [130.4.1, 130.4.2]
func TestRespondSessionLost_StructuredJSON(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","id":42,"method":"tools/call","params":{"name":"neo_memory","arguments":{"action":"learn","directive":"x"}}}`)
	req := httptest.NewRequest(http.MethodPost, "/mcp/message?sessionId=expired-abc", strings.NewReader(string(body)))
	req.Header.Set("X-Neo-Workspace", "neoanvil-45913")
	rr := httptest.NewRecorder()

	respondSessionLost(rr, req, body, "expired-abc")

	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404 (so SDK reconnects), got %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("expected JSON content-type, got %q", ct)
	}

	var envelope struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    struct {
				SuggestFallbackCurl bool   `json:"suggest_fallback_curl"`
				FallbackCurl        string `json:"fallback_curl"`
				SessionID           string `json:"session_id"`
			} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("response is not valid JSON: %v\nbody=%s", err, rr.Body.String())
	}
	if envelope.JSONRPC != "2.0" {
		t.Errorf("expected jsonrpc=2.0, got %q", envelope.JSONRPC)
	}
	if string(envelope.ID) != "42" {
		t.Errorf("expected id echoed (42), got %s", envelope.ID)
	}
	if envelope.Error.Code != -32001 {
		t.Errorf("expected error.code=-32001 (custom session-lost), got %d", envelope.Error.Code)
	}
	if !envelope.Error.Data.SuggestFallbackCurl {
		t.Errorf("expected suggest_fallback_curl=true (programmatic flag for agent)")
	}
	if envelope.Error.Data.FallbackCurl == "" {
		t.Errorf("expected non-empty fallback_curl")
	}
	if !strings.Contains(envelope.Error.Data.FallbackCurl, "X-Neo-Workspace: neoanvil-45913") {
		t.Errorf("fallback_curl missing X-Neo-Workspace header: %s", envelope.Error.Data.FallbackCurl)
	}
	if !strings.Contains(envelope.Error.Data.FallbackCurl, "neo_memory") {
		t.Errorf("fallback_curl missing original payload: %s", envelope.Error.Data.FallbackCurl)
	}
	if envelope.Error.Data.SessionID != "expired-abc" {
		t.Errorf("expected session_id echoed, got %q", envelope.Error.Data.SessionID)
	}
}

// TestRespondSessionLost_NullID verifies the helper handles malformed/missing
// JSON-RPC id without panicking — JSON-RPC spec allows null id.
func TestRespondSessionLost_NullID(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","method":"ping"}`) // no id field
	req := httptest.NewRequest(http.MethodPost, "/mcp/message?sessionId=lost", strings.NewReader(string(body)))
	rr := httptest.NewRecorder()

	respondSessionLost(rr, req, body, "lost")

	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"id":null`) {
		t.Errorf("expected id rendered as null, got body=%s", rr.Body.String())
	}
}

// TestRespondSessionLost_BodyWithSingleQuotes verifies bash-quote escaping
// inside fallback_curl handles payloads that contain single quotes.
func TestRespondSessionLost_BodyWithSingleQuotes(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"arguments":{"q":"it's a quote"}}}`)
	req := httptest.NewRequest(http.MethodPost, "/mcp/message?sessionId=x", strings.NewReader(string(body)))
	rr := httptest.NewRecorder()

	respondSessionLost(rr, req, body, "x")

	var envelope struct {
		Error struct {
			Data struct {
				FallbackCurl string `json:"fallback_curl"`
			} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(envelope.Error.Data.FallbackCurl, `it'\''s a quote`) {
		t.Errorf("expected bash-escaped single quote (it'\\''s), got %s", envelope.Error.Data.FallbackCurl)
	}
}
