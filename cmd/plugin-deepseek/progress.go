package main

// EmitProgress sends a JSON-RPC 2.0 notifications/progress event via the plugin's notify channel.
// No-op when notifyFn or token is nil.
func EmitProgress(notifyFn func(map[string]any), token any, n, total int64, msg string) {
	if notifyFn == nil || token == nil {
		return
	}
	params := map[string]any{
		"progressToken": token,
		"progress":      n,
		"total":         total,
	}
	if msg != "" {
		params["message"] = msg
	}
	notifyFn(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/progress",
		"params":  params,
	})
}
