// pkg/darwin/profiler.go — Fitness Profiler for genetic code evolution. [SRE-93.A]
//
// Identifies the most CPU-intensive function in the last 24h using Oracle
// metrics, extracts its source code via go/ast, and prepares a benchmark
// harness for the mutation engine.
package darwin

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"sort"
	"strings"
	"time"
)

// Metric represents a single function's runtime profile data from Oracle.
type Metric struct {
	Package   string
	Function  string
	File      string
	Line      int
	CPUSec    float64
	AllocBytes int64
	CallCount int64
	Timestamp time.Time
}

// Hotspot is the selected function for evolutionary optimization. [SRE-93.A.1]
type Hotspot struct {
	Package    string  `json:"package"`
	Function   string  `json:"function"`
	File       string  `json:"file"`
	Line       int     `json:"line"`
	CPUSeconds float64 `json:"cpu_seconds"`
	AllocBytes int64   `json:"alloc_bytes"`
	CallCount  int64   `json:"call_count"`
}

// SelectHotspot ranks functions by CPU consumption and returns the top candidate.
// Excludes test files, vendor, and runtime packages. [SRE-93.A.1]
func SelectHotspot(metrics []Metric) (Hotspot, bool) {
	// Filter and aggregate by function.
	type key struct{ pkg, fn string }
	agg := make(map[key]*Metric)

	for i := range metrics {
		m := &metrics[i]
		// Exclude test files, vendor, runtime.
		if strings.HasSuffix(m.File, "_test.go") {
			continue
		}
		if strings.Contains(m.Package, "vendor/") || strings.HasPrefix(m.Package, "runtime") {
			continue
		}
		k := key{m.Package, m.Function}
		if existing, ok := agg[k]; ok {
			existing.CPUSec += m.CPUSec
			existing.AllocBytes += m.AllocBytes
			existing.CallCount += m.CallCount
		} else {
			cpy := *m
			agg[k] = &cpy
		}
	}

	if len(agg) == 0 {
		return Hotspot{}, false
	}

	// Sort by CPU seconds descending.
	ranked := make([]*Metric, 0, len(agg))
	for _, m := range agg {
		ranked = append(ranked, m)
	}
	sort.Slice(ranked, func(i, j int) bool {
		return ranked[i].CPUSec > ranked[j].CPUSec
	})

	top := ranked[0]
	return Hotspot{
		Package:    top.Package,
		Function:   top.Function,
		File:       top.File,
		Line:       top.Line,
		CPUSeconds: top.CPUSec,
		AllocBytes: top.AllocBytes,
		CallCount:  top.CallCount,
	}, true
}

// ExtractFunction extracts a named function's source from a Go file using go/ast.
// Returns the complete function source and its required imports. [SRE-93.A.2]
func ExtractFunction(filePath, funcName string) (source string, imports []string, err error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filePath, nil, parser.ParseComments)
	if err != nil {
		return "", nil, fmt.Errorf("parse %s: %w", filePath, err)
	}

	// Collect imports.
	for _, imp := range f.Imports {
		path := imp.Path.Value // already quoted
		if imp.Name != nil {
			imports = append(imports, imp.Name.Name+" "+path)
		} else {
			imports = append(imports, path)
		}
	}

	// Find the target function.
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != funcName {
			continue
		}
		// Print the function source.
		var buf strings.Builder
		if err := printer.Fprint(&buf, fset, fn); err != nil {
			return "", nil, fmt.Errorf("print func %s: %w", funcName, err)
		}
		return buf.String(), imports, nil
	}

	return "", nil, fmt.Errorf("function %s not found in %s", funcName, filePath)
}

// BenchmarkResult captures the performance of a single variant.
type BenchmarkResult struct {
	NsPerOp    int64 `json:"ns_per_op"`
	AllocsPerOp int64 `json:"allocs_per_op"`
	Compiled   bool  `json:"compiled"`
	Error      string `json:"error,omitempty"`
}

// GenerateBenchmarkSource wraps a function in a standalone Go file with a
// benchmark main. The caller should compile and run this in a sandbox. [SRE-93.A.3]
func GenerateBenchmarkSource(funcSource string, imports []string, iterations int) string {
	if iterations <= 0 {
		iterations = 1
	}
	var sb strings.Builder
	sb.WriteString("package main\n\nimport (\n")
	sb.WriteString("\t\"fmt\"\n")
	sb.WriteString("\t\"runtime\"\n")
	sb.WriteString("\t\"time\"\n")
	for _, imp := range imports {
		if imp == `"fmt"` || imp == `"time"` || imp == `"runtime"` {
			continue
		}
		sb.WriteString("\t")
		sb.WriteString(imp)
		sb.WriteString("\n")
	}
	sb.WriteString(")\n\n")
	sb.WriteString(funcSource)
	sb.WriteString("\n\n")
	fmt.Fprintf(&sb, "func main() {\n")
	fmt.Fprintf(&sb, "\tvar m1, m2 runtime.MemStats\n")
	fmt.Fprintf(&sb, "\truntime.ReadMemStats(&m1)\n")
	fmt.Fprintf(&sb, "\tstart := time.Now()\n")
	fmt.Fprintf(&sb, "\tfor i := 0; i < %d; i++ {\n", iterations)
	fmt.Fprintf(&sb, "\t\t// caller must replace this with actual invocation\n")
	fmt.Fprintf(&sb, "\t}\n")
	fmt.Fprintf(&sb, "\telapsed := time.Since(start)\n")
	fmt.Fprintf(&sb, "\truntime.ReadMemStats(&m2)\n")
	fmt.Fprintf(&sb, "\tnsPerOp := elapsed.Nanoseconds() / int64(%d)\n", iterations)
	fmt.Fprintf(&sb, "\tallocs := int64(m2.TotalAlloc - m1.TotalAlloc) / int64(%d)\n", iterations)
	fmt.Fprintf(&sb, "\tfmt.Printf(\"%%d %%d\\n\", nsPerOp, allocs)\n")
	sb.WriteString("}\n")
	return sb.String()
}

// ParseBenchmarkOutput parses the "nsPerOp allocsPerOp" line from the benchmark binary.
func ParseBenchmarkOutput(output string) (nsPerOp, allocsPerOp int64, err error) {
	_, err = fmt.Sscanf(strings.TrimSpace(output), "%d %d", &nsPerOp, &allocsPerOp)
	return
}

// WriteTempFile writes content to a temporary file and returns its path.
func WriteTempFile(content, prefix, ext string) (string, error) {
	f, err := os.CreateTemp("", prefix+"*"+ext)
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	f.Close()
	return f.Name(), nil
}
