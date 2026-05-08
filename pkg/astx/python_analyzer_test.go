package astx

import (
	"context"
	"strings"
	"testing"
)

// TestPythonAnalyzer_CleanLowCC verifies a simple function produces zero error findings.
// CC_SUMMARY findings are allowed (informational only).
// [Épica 247.C]
func TestPythonAnalyzer_CleanLowCC(t *testing.T) {
	src := `def add(x, y):
    return x + y

def greet(name):
    if name:
        return f"hello {name}"
    return "hello world"
`
	findings, err := PythonAnalyzer{}.Analyze(context.Background(), "clean.py", []byte(src))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	for _, f := range findings {
		if f.Kind != "CC_SUMMARY" {
			t.Errorf("expected no error findings, got: %+v", f)
		}
	}
	// Verify CC_SUMMARY is present with function CC data.
	hasSummary := false
	for _, f := range findings {
		if f.Kind == "CC_SUMMARY" {
			hasSummary = true
			if !strings.Contains(f.Message, "add:") || !strings.Contains(f.Message, "greet:") {
				t.Errorf("CC_SUMMARY missing function entries: %s", f.Message)
			}
		}
	}
	if !hasSummary {
		t.Error("expected CC_SUMMARY finding to be present")
	}
}

// TestPythonAnalyzer_ComplexCC16 verifies a CC=16+ function is flagged.
// [Épica 247.C]
func TestPythonAnalyzer_ComplexCC16(t *testing.T) {
	// Each if/elif/for/while/and/or adds 1. We craft a body with 18 decision points.
	src := `def complex_router(req, state, cache, db, q):
    if req.method == "GET" and state.ready and cache.warm:
        for item in req.items:
            if item.valid and item.fresh:
                pass
            elif item.retry and item.queued:
                pass
            elif item.skip or item.cancel:
                pass
    elif req.method == "POST" or req.method == "PUT" or req.method == "PATCH":
        while state.queue and not state.halt:
            for m in q.messages:
                if m.ready and m.sender:
                    pass
                elif m.retry:
                    pass
    elif req.method == "DELETE":
        for d in db.records:
            if d.soft and d.owned:
                pass
    return state
`
	findings, err := PythonAnalyzer{}.Analyze(context.Background(), "complex.py", []byte(src))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	foundCC := false
	for _, f := range findings {
		if f.Kind == "COMPLEXITY" && strings.Contains(f.Message, "complex_router") {
			foundCC = true
			break
		}
	}
	if !foundCC {
		t.Errorf("expected COMPLEXITY finding for complex_router, got: %+v", findings)
	}
}

// TestPythonAnalyzer_ShadowFor — a `for x in ...` where x is a parameter.
// [Épica 247.C]
func TestPythonAnalyzer_ShadowFor(t *testing.T) {
	src := `def process(item, items):
    for item in items:
        print(item)
`
	findings, err := PythonAnalyzer{}.Analyze(context.Background(), "shadow.py", []byte(src))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	foundShadow := false
	for _, f := range findings {
		if f.Kind == "SHADOW" && strings.Contains(f.Message, "item") {
			foundShadow = true
			break
		}
	}
	if !foundShadow {
		t.Errorf("expected SHADOW finding for 'item' (param), got: %+v", findings)
	}
}

// TestPythonAnalyzer_ShadowExcept — `except X as e` where e is a param.
// [Épica 247.C]
func TestPythonAnalyzer_ShadowExcept(t *testing.T) {
	src := `def handle(e):
    try:
        risky()
    except Exception as e:
        log(e)
`
	findings, err := PythonAnalyzer{}.Analyze(context.Background(), "shadow_except.py", []byte(src))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	foundShadow := false
	for _, f := range findings {
		if f.Kind == "SHADOW" && strings.Contains(f.Message, "'e'") {
			foundShadow = true
			break
		}
	}
	if !foundShadow {
		t.Errorf("expected SHADOW finding for 'e' param, got: %+v", findings)
	}
}

// TestPythonAnalyzer_ShadowWith — `with X as f` where f is a param.
// [Épica 247.C]
func TestPythonAnalyzer_ShadowWith(t *testing.T) {
	src := `def read_config(f):
    with open("x.yaml") as f:
        return f.read()
`
	findings, err := PythonAnalyzer{}.Analyze(context.Background(), "shadow_with.py", []byte(src))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	foundShadow := false
	for _, f := range findings {
		if f.Kind == "SHADOW" && strings.Contains(f.Message, "'f'") {
			foundShadow = true
			break
		}
	}
	if !foundShadow {
		t.Errorf("expected SHADOW finding for 'f' param, got: %+v", findings)
	}
}

// TestPythonAnalyzer_NoShadowWhenNotParam — loop var that's NOT a parameter.
// [Épica 247.C]
func TestPythonAnalyzer_NoShadowWhenNotParam(t *testing.T) {
	src := `def count_items(items):
    total = 0
    for item in items:
        total += 1
    return total
`
	findings, err := PythonAnalyzer{}.Analyze(context.Background(), "no_shadow.py", []byte(src))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	for _, f := range findings {
		if f.Kind == "SHADOW" {
			t.Errorf("unexpected SHADOW: %+v", f)
		}
	}
}

// TestPythonAnalyzer_StripsSelfAndCls ensures self/cls in method signatures
// are not treated as shadowable parameters.
// [Épica 247.C]
func TestPythonAnalyzer_StripsSelfAndCls(t *testing.T) {
	src := `class MyCls:
    def method(self, x):
        for self in []:
            pass
`
	findings, err := PythonAnalyzer{}.Analyze(context.Background(), "self_cls.py", []byte(src))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	for _, f := range findings {
		if f.Kind == "SHADOW" && strings.Contains(f.Message, "self") {
			t.Errorf("self should not be tracked as shadowable param: %+v", f)
		}
	}
}

// TestPythonAnalyzer_StripsTypeHintsAndDefaults verifies that `x: int = 0`
// param still registers as `x`.
// [Épica 247.C]
func TestPythonAnalyzer_StripsTypeHintsAndDefaults(t *testing.T) {
	src := `def fn(x: int = 0):
    for x in []:
        pass
`
	findings, err := PythonAnalyzer{}.Analyze(context.Background(), "hints.py", []byte(src))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	foundShadow := false
	for _, f := range findings {
		if f.Kind == "SHADOW" && strings.Contains(f.Message, "'x'") {
			foundShadow = true
			break
		}
	}
	if !foundShadow {
		t.Errorf("expected SHADOW for 'x' despite type hint + default, got: %+v", findings)
	}
}
