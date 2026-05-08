package astx

import (
	"strings"
	"testing"
)

// TestAuditGoFile_TickerPattern_NoFalsePositive verifies that a for{} whose body
// is only a channel receive (<-ticker.C) is NOT flagged as INFINITE_LOOP.
func TestAuditGoFile_TickerPattern_NoFalsePositive(t *testing.T) {
	src := `package main
import "time"
func worker() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		<-ticker.C
	}
}`
	findings, err := AuditGoFile("ticker_test.go", []byte(src))
	if err != nil {
		t.Fatalf("AuditGoFile: %v", err)
	}
	for _, f := range findings {
		if f.Kind == "INFINITE_LOOP" {
			t.Errorf("false positive: ticker for{} flagged as INFINITE_LOOP at line %d", f.Line)
		}
	}
}

// TestAuditGoFile_TrueInfiniteLoop verifies that a truly empty for{} IS flagged.
func TestAuditGoFile_TrueInfiniteLoop(t *testing.T) {
	src := `package main
func spin() {
	for {
		_ = 1
	}
}`
	findings, err := AuditGoFile("spin_test.go", []byte(src))
	if err != nil {
		t.Fatalf("AuditGoFile: %v", err)
	}
	found := false
	for _, f := range findings {
		if f.Kind == "INFINITE_LOOP" {
			found = true
		}
	}
	if !found {
		t.Error("expected INFINITE_LOOP for empty spin loop, got none")
	}
}

// TestAuditGoFile_SelectTickerPattern_NoFalsePositive covers the common
// select { case <-ticker.C: ... } pattern inside a for{}.
func TestAuditGoFile_SelectTickerPattern_NoFalsePositive(t *testing.T) {
	src := `package main
import "time"
func run(done <-chan struct{}) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
		}
	}
}`
	findings, err := AuditGoFile("select_ticker_test.go", []byte(src))
	if err != nil {
		t.Fatalf("AuditGoFile: %v", err)
	}
	for _, f := range findings {
		if f.Kind == "INFINITE_LOOP" {
			t.Errorf("false positive: select ticker for{} flagged as INFINITE_LOOP at line %d: %s", f.Line, f.Message)
		}
	}
}

// TestAuditGoFile_ForSelectPattern_NoFalsePositive covers for { select { ... } } event loops.
func TestAuditGoFile_ForSelectPattern_NoFalsePositive(t *testing.T) {
	src := `package main
func writePump(send <-chan []byte, done <-chan struct{}) {
	for {
		select {
		case msg, ok := <-send:
			if !ok {
				return
			}
			_ = msg
		case <-done:
			return
		}
	}
}`
	findings, err := AuditGoFile("writepump_test.go", []byte(src))
	if err != nil {
		t.Fatalf("AuditGoFile: %v", err)
	}
	for _, f := range findings {
		if f.Kind == "INFINITE_LOOP" {
			t.Errorf("false positive: for{select{}} flagged at line %d", f.Line)
		}
	}
}

// TestAuditGoFile_AcceptLoop_NoFalsePositive covers server accept loops.
func TestAuditGoFile_AcceptLoop_NoFalsePositive(t *testing.T) {
	src := `package main
import "net"
func serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		go handle(conn)
	}
}
func handle(c net.Conn) { c.Close() }`
	findings, err := AuditGoFile("accept_test.go", []byte(src))
	if err != nil {
		t.Fatalf("AuditGoFile: %v", err)
	}
	for _, f := range findings {
		if f.Kind == "INFINITE_LOOP" {
			t.Errorf("false positive: accept loop flagged at line %d", f.Line)
		}
	}
}

// TestAuditGoFile_SleepLoop_NoFalsePositive covers for { time.Sleep(...) } polling loops.
func TestAuditGoFile_SleepLoop_NoFalsePositive(t *testing.T) {
	src := `package main
import "time"
func poll() {
	for {
		check()
		time.Sleep(time.Second)
	}
}
func check() {}`
	findings, err := AuditGoFile("sleep_test.go", []byte(src))
	if err != nil {
		t.Fatalf("AuditGoFile: %v", err)
	}
	for _, f := range findings {
		if f.Kind == "INFINITE_LOOP" {
			t.Errorf("false positive: sleep loop flagged at line %d", f.Line)
		}
	}
}

// TestAuditGoFile_Complexity_OK verifies no complexity finding for a simple func.
func TestAuditGoFile_Complexity_OK(t *testing.T) {
	src := `package main
func add(a, b int) int { return a + b }`
	findings, err := AuditGoFile("simple_test.go", []byte(src))
	if err != nil {
		t.Fatalf("AuditGoFile: %v", err)
	}
	for _, f := range findings {
		if f.Kind == "COMPLEXITY" {
			t.Errorf("unexpected COMPLEXITY finding: %s", f.Message)
		}
	}
}

// TestShadowLoop_Sequential_NoFP verifies that two sequential range loops using the
// same variable names do NOT produce false-positive SHADOW findings. [160.B]
func TestShadowLoop_Sequential_NoFP(t *testing.T) {
	src := `package main
func process(items []string) {
	for i, v := range items {
		result := len(v)
		_ = i
		_ = result
	}
	for i, v := range items {
		result := len(v)
		_ = i
		_ = result
	}
}`
	findings, err := AuditGoFile("seq_loops_test.go", []byte(src))
	if err != nil {
		t.Fatalf("AuditGoFile: %v", err)
	}
	for _, f := range findings {
		if f.Kind == "SHADOW" {
			t.Errorf("false positive SHADOW in sequential range loops: %s (line %d)", f.Message, f.Line)
		}
	}
}

// TestShadowLoop_Nested_RealShadow verifies that a nested range loop whose key
// shadows the outer range key IS flagged. [160.B]
func TestShadowLoop_Nested_RealShadow(t *testing.T) {
	src := `package main
func nested(matrix [][]int) {
	for i, row := range matrix {
		for i, val := range row {
			_ = i
			_ = val
		}
		_ = row
	}
}`
	findings, err := AuditGoFile("nested_loops_test.go", []byte(src))
	if err != nil {
		t.Fatalf("AuditGoFile: %v", err)
	}
	found := false
	for _, f := range findings {
		if f.Kind == "SHADOW" && strings.Contains(f.Message, "'i'") {
			found = true
		}
	}
	if !found {
		t.Error("expected SHADOW for inner 'i' shadowing outer range 'i', got none")
	}
}

// TestShadowFunc_ParamShadow verifies that a variable declared in a range body that
// shadows a function parameter IS still flagged after the sequential-loop FP fix. [160.B]
func TestShadowFunc_ParamShadow(t *testing.T) {
	src := `package main
func transform(err error, items []string) []string {
	results := make([]string, 0, len(items))
	for _, item := range items {
		err := validate(item)
		if err != nil {
			continue
		}
		results = append(results, item)
	}
	_ = err
	return results
}
func validate(s string) error { return nil }`
	findings, err := AuditGoFile("param_shadow_test.go", []byte(src))
	if err != nil {
		t.Fatalf("AuditGoFile: %v", err)
	}
	found := false
	for _, f := range findings {
		if f.Kind == "SHADOW" && strings.Contains(f.Message, "'err'") {
			found = true
		}
	}
	if !found {
		t.Error("expected SHADOW for 'err' shadowing the param, got none")
	}
}

// TestAuditGoFile_ShadowVar detects a shadowed variable.
func TestAuditGoFile_ShadowVar(t *testing.T) {
	src := `package main
func f() {
	err := doA()
	if err != nil {
		err := doB()
		_ = err
	}
	_ = err
}
func doA() error { return nil }
func doB() error { return nil }`
	findings, err := AuditGoFile("shadow_test.go", []byte(src))
	if err != nil {
		t.Fatalf("AuditGoFile: %v", err)
	}
	found := false
	for _, f := range findings {
		if f.Kind == "SHADOW" && strings.Contains(f.Message, "err") {
			found = true
		}
	}
	if !found {
		t.Error("expected SHADOW finding for 'err', got none")
	}
}

// TestShadowLoop_GoroutineCapture_NoFP verifies that capturing the range
// variable from a goroutine body is NOT flagged as SHADOW. In Go 1.22+ each
// iteration gets its own copy, so the classic capture bug is gone — the
// detector must not misinterpret the capture as a shadow. [330.D]
func TestShadowLoop_GoroutineCapture_NoFP(t *testing.T) {
	src := `package main
func spawn(items []int) {
	for i := range items {
		go func() { _ = i }()
	}
}`
	findings, err := AuditGoFile("go122_capture_test.go", []byte(src))
	if err != nil {
		t.Fatalf("AuditGoFile: %v", err)
	}
	for _, f := range findings {
		if f.Kind == "SHADOW" {
			t.Errorf("false positive SHADOW for loopvar capture (Go 1.22+ semantic): %s (line %d)", f.Message, f.Line)
		}
	}
}

// TestShadowLoop_ForInitShadowsOuter verifies that the init stmt of a 3-clause
// `for` that declares a var with `:=` matching an outer scope name IS flagged.
// Go 1.22+ per-iteration semantics do not change this shadow rule. [330.D]
func TestShadowLoop_ForInitShadowsOuter(t *testing.T) {
	src := `package main
func threeClause(i int) {
	for i := 0; i < 5; i++ {
		_ = i
	}
}`
	findings, err := AuditGoFile("go122_3clause_test.go", []byte(src))
	if err != nil {
		t.Fatalf("AuditGoFile: %v", err)
	}
	found := false
	for _, f := range findings {
		if f.Kind == "SHADOW" && strings.Contains(f.Message, "'i'") {
			found = true
		}
	}
	if !found {
		t.Error("expected SHADOW for 3-clause for init shadowing param 'i', got none")
	}
}

// TestShadowLoop_RangeAssign_NoFP verifies that a range loop using `=`
// (assignment, not declaration) on pre-existing outer vars is NOT flagged.
// The detector's SHADOW semantics only trigger for `:=`. [330.D]
func TestShadowLoop_RangeAssign_NoFP(t *testing.T) {
	src := `package main
func assignRange(items []int) {
	var i, v int
	for i, v = range items {
		_ = i
		_ = v
	}
	_ = i
	_ = v
}`
	findings, err := AuditGoFile("go122_range_assign_test.go", []byte(src))
	if err != nil {
		t.Fatalf("AuditGoFile: %v", err)
	}
	for _, f := range findings {
		if f.Kind == "SHADOW" {
			t.Errorf("false positive SHADOW on range-assign (no :=): %s (line %d)", f.Message, f.Line)
		}
	}
}

// TestShadowLoop_ObsoleteReDeclareIdiom_Flagged verifies that the pre-Go 1.22
// `i := i` idiom inside a range body — used historically to defeat the
// capture bug — IS still flagged as SHADOW in Go 1.22+ since it's now
// obsolete (redundant shadowing, encourages unnecessary copies). [330.D]
func TestShadowLoop_ObsoleteReDeclareIdiom_Flagged(t *testing.T) {
	src := `package main
func obsoleteIdiom(items []int) {
	for i := range items {
		i := i
		go func() { _ = i }()
	}
}`
	findings, err := AuditGoFile("go122_obsolete_idiom_test.go", []byte(src))
	if err != nil {
		t.Fatalf("AuditGoFile: %v", err)
	}
	found := false
	for _, f := range findings {
		if f.Kind == "SHADOW" && strings.Contains(f.Message, "'i'") {
			found = true
		}
	}
	if !found {
		t.Error("expected SHADOW for obsolete 'i := i' capture idiom in Go 1.22+, got none")
	}
}
