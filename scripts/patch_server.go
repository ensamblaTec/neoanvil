package main

import (
	"os"
	"strings"
)

func main() {
	b, err := os.ReadFile("../cmd/neo-mcp/server.go")
	if err != nil {
		panic(err)
	}

	s := string(b)
	s = strings.Replace(s, "scanner           *bufio.Scanner", "decoder           *json.Decoder", 1)
	
	idx := strings.Index(s, "scanner := bufio.NewScanner(os.Stdin)")
	if idx > 0 {
		endIdx := strings.Index(s[idx:], "return &MCPServer{")
		if endIdx > 0 {
			replacement := "decoder := json.NewDecoder(io.LimitReader(os.Stdin, 50*1024*1024))\n\n\treturn &MCPServer{"
			s = s[:idx] + replacement + s[idx+endIdx+18:]
		}
	}
	
	s = strings.Replace(s, "scanner: scanner,", "decoder: decoder,", 1)
	
	idxLoop := strings.Index(s, "for mcp.scanner.Scan() {")
	if idxLoop > 0 {
		endLoopReq := strings.Index(s[idxLoop:], "isNotif := false")
		if endLoopReq > 0 {
			replacement := `for {
		var req RPCRequest
		if err := mcp.decoder.Decode(&req); err != nil {
			break
		}
		
		`
			s = s[:idxLoop] + replacement + s[idxLoop+endLoopReq:]
		}
	}

	idxErr := strings.Index(s, "if err := mcp.scanner.Err()")
	if idxErr > 0 {
		endErr := strings.Index(s[idxErr:], "}\n\tmcp.wg.Wait()")
		if endErr > 0 {
			s = s[:idxErr] + s[idxErr+endErr+2:]
		}
	}
	
	os.WriteFile("../cmd/neo-mcp/server.go", []byte(s), 0644)
}