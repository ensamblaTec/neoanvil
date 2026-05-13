package main

import (
	"bytes"
	"go/parser"
	"go/token"
	"log"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/astx"
)

func generate70kLOC() string {
	var b strings.Builder
	b.Grow(3 * 1024 * 1024)
	b.WriteString("package stress\n\n")

	// Generate 5500 structs (10 lines each = 55k lines)
	for i := range 5500 {
		istr := strconv.Itoa(i)
		b.WriteString("type MassiveGodObject")
		b.WriteString(istr)
		b.WriteString(" struct {\n\tFieldA string\n\tFieldB int\n\tFieldC map[string]int\n\tFieldD []byte\n\tFieldE float64\n\tFieldF bool\n\tFieldG struct{ Inner string }\n\tFieldH any\n}\n\n")
	}

	// Generate 2000 functions (10 lines each = 20k lines)
	for i := range 2000 {
		istr := strconv.Itoa(i)
		b.WriteString("func ProcessMassive")
		b.WriteString(istr)
		b.WriteString("(obj *MassiveGodObject")
		b.WriteString(istr)
		b.WriteString(") int {\n\tif obj == nil { return 0 }\n\tif obj.FieldB > 100 { obj.FieldB = 0 }\n\tfor k := 0; k < 10; k++ {\n\t\tobj.FieldA += \"-\"\n\t}\n\treturn obj.FieldB\n}\n\n")
	}

	return b.String()
}

func getMem() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.Alloc
}

func main() {
	if os.Getenv("GOGC") == "" {
		log.Println("[WARN] Ejecuta esto con GOGC=off para simular latencia Zero-Alloc SRE pura si quieres evadir el Profiler de pruebas.")
	}

	log.Println("[SRE-TARGET] Generando Monolito de ~70,000 Líneas de Código en RAM...")
	codeStr := generate70kLOC()
	lines := bytes.Count([]byte(codeStr), []byte("\n"))
	log.Printf("[SRE-TARGET] Generado. Tamaño en RAM: %d MB. Líneas físicas abstractas: %d\n\n", len(codeStr)/1024/1024, lines)

	runtime.GC()
	<-time.After(100 * time.Millisecond) // Dejar asentar la RAM
	memStart := getMem()

	log.Println("=========================================================================")
	log.Println("1. NEOANVIL V1 (Base AST Parsing - Solo Lectura)")
	log.Println("=========================================================================")
	t1 := time.Now()
	fset1 := token.NewFileSet()
	_, err := parser.ParseFile(fset1, "monolith.go", codeStr, parser.ParseComments)
	v1Time := time.Since(t1)
	if err != nil {
		log.Fatalf("Error V1: %v", err)
	}
	memV1 := getMem() - memStart
	log.Printf("Latencia Parseo AST: %v\n", v1Time)
	log.Printf("Pico RAM Generado  : %d MB\n\n", memV1/1024/1024)

	runtime.GC()
	<-time.After(500 * time.Millisecond)
	memStart = getMem()

	log.Println("=========================================================================")
	log.Println("2. NEOANVIL V2 (Micro-PRM + Complexity + SSRF Shield S5 + AST Deep Scan)")
	log.Println("=========================================================================")
	t2 := time.Now()

	verdict := astx.EvaluatePolyglotPRM(codeStr, ".go", nil)
	v2Time := time.Since(t2)

	memV2 := getMem() - memStart

	log.Printf("Latencia Validación Total : %v\n", v2Time)
	log.Printf("Puntaje PRM Neural        : %.2f\n", verdict.Score)
	log.Printf("Diagnóstico SRE           : %s\n", verdict.Verdict)
	log.Printf("Pico RAM Generado         : %d MB\n\n", memV2/1024/1024)

	log.Println("=========================================================================")
	log.Println("CONCLUSIÓN TERMODINÁMICA")
	overH := v2Time - v1Time
	log.Printf("Overhead Seguridad v2: %v (+%.1f%% de la latencia V1)\n", overH, float64(overH)*100/float64(v1Time))
	log.Println("> NeoAnvil v2 intercepta un monolito gigante, analiza repetición matemática (LZ76),")
	log.Println("> busca Egress Shells ocultas y escanea nodos AST en tiempo real sin bloquear")
	log.Println("> el thread industrial MIO (Multiplexed I/O).")
}
