package astx

import (
	"context"
	"strings"
	"testing"
)

// TestRustAnalyzer_CleanLowCC — zero findings on simple fn.
// [Épica 247.E]
func TestRustAnalyzer_CleanLowCC(t *testing.T) {
	src := `fn add(a: i32, b: i32) -> i32 {
    a + b
}

fn greet(name: &str) -> String {
    if name.is_empty() {
        String::from("hello world")
    } else {
        format!("hello {}", name)
    }
}
`
	findings, err := RustAnalyzer{}.Analyze(context.Background(), "clean.rs", []byte(src))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("expected 0 findings, got %d: %+v", len(findings), findings)
	}
}

// TestRustAnalyzer_ComplexCC16 — CC≥16 triggers COMPLEXITY.
// [Épica 247.E]
func TestRustAnalyzer_ComplexCC16(t *testing.T) {
	src := `fn complex_router(req: &Request, state: &mut State, cache: &Cache) -> Result<(), Error> {
    if req.method == "GET" && state.ready && cache.warm {
        for item in req.items.iter() {
            match item.kind {
                Kind::A => {},
                Kind::B => {},
                Kind::C => {},
                Kind::D => {},
                _ => {},
            }
            if item.valid && item.fresh {
                continue;
            } else if item.retry || item.queued {
                continue;
            }
        }
    } else if req.method == "POST" || req.method == "PUT" {
        while state.queue.len() > 0 && !state.halt {
            loop {
                if state.done { break; }
            }
        }
    }
    Ok(())
}
`
	findings, err := RustAnalyzer{}.Analyze(context.Background(), "complex.rs", []byte(src))
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

// TestRustAnalyzer_IntentionalShadowing — rebinding same name reports INFO, not error.
// [Épica 247.E]
func TestRustAnalyzer_IntentionalShadowing(t *testing.T) {
	src := `fn transform(input: &str) -> String {
    let x = 1;
    let x = x + 1;
    let x = x.to_string();
    x
}
`
	findings, err := RustAnalyzer{}.Analyze(context.Background(), "shadow.rs", []byte(src))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	infoCount := 0
	for _, f := range findings {
		if f.Kind == "SHADOW_INFO" {
			infoCount++
		}
		if f.Kind == "SHADOW" {
			t.Errorf("Rust intentional shadowing must be SHADOW_INFO, not SHADOW: %+v", f)
		}
	}
	if infoCount < 2 {
		t.Errorf("expected ≥2 SHADOW_INFO entries (second and third let x), got %d: %+v", infoCount, findings)
	}
}

// TestRustAnalyzer_UnderscoreIgnored — `let _ = ...` is not tracked.
// [Épica 247.E]
func TestRustAnalyzer_UnderscoreIgnored(t *testing.T) {
	src := `fn fn_with_underscore() {
    let _ = do_something();
    let _ = do_more();
}
`
	findings, err := RustAnalyzer{}.Analyze(context.Background(), "underscore.rs", []byte(src))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	for _, f := range findings {
		if f.Kind == "SHADOW_INFO" {
			t.Errorf("'_' should not be tracked as shadow: %+v", f)
		}
	}
}

// TestRustAnalyzer_MutBinding — `let mut x =` counts the same as `let x =`.
// [Épica 247.E]
func TestRustAnalyzer_MutBinding(t *testing.T) {
	src := `fn fn_mut() {
    let mut x = 0;
    let x = x + 1;
    println!("{}", x);
}
`
	findings, err := RustAnalyzer{}.Analyze(context.Background(), "mut.rs", []byte(src))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	foundInfo := false
	for _, f := range findings {
		if f.Kind == "SHADOW_INFO" && strings.Contains(f.Message, "'x'") {
			foundInfo = true
		}
	}
	if !foundInfo {
		t.Errorf("expected SHADOW_INFO for 'x' rebind after mut, got: %+v", findings)
	}
}

// TestRustAnalyzer_LineCommentsStripped — `//` comments don't affect analysis.
// [Épica 247.E]
func TestRustAnalyzer_LineCommentsStripped(t *testing.T) {
	src := `fn fn_comments() {
    let x = 1;
    // let x = 2; // comment shouldn't trigger shadow
    x
}
`
	findings, err := RustAnalyzer{}.Analyze(context.Background(), "comments.rs", []byte(src))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	for _, f := range findings {
		if f.Kind == "SHADOW_INFO" {
			t.Errorf("commented let should not shadow: %+v", f)
		}
	}
}

// TestRustAnalyzer_AsyncFnDetected — `async fn name()` should register.
// [Épica 247.E]
func TestRustAnalyzer_AsyncFnDetected(t *testing.T) {
	src := `async fn fetch(url: &str) -> Result<String, Error> {
    if url.is_empty() {
        return Err(Error::EmptyUrl);
    }
    Ok(String::from(url))
}
`
	findings, err := RustAnalyzer{}.Analyze(context.Background(), "async.rs", []byte(src))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	// Should produce no findings for this simple async fn (CC=2).
	if len(findings) != 0 {
		t.Errorf("expected 0 findings on simple async fn, got %+v", findings)
	}
}
