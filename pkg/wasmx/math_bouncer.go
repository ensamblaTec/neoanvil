package wasmx

import (
	"context"
	"fmt"
	"math"

	"github.com/ensamblatec/neoanvil/pkg/rag"
	"github.com/ensamblatec/neoanvil/pkg/tensorx"
)

var mathMlp *tensorx.MLP

func init() {
	// Fallback weights just in case WAL is unmounted
	w1Data := []float32{
		0.5, 0.2, 0.5, 0.2,
		-0.5, -0.2, -0.4, -0.5,
		-0.1, -0.05, -0.1, -0.05,
		-0.8, -0.6, -0.8, -0.7,
		-0.6, -0.5, -0.7, -0.6,
	}
	w1, _ := tensorx.NewTensor(w1Data, tensorx.Shape{5, 4})

	w2Data := []float32{0.25, 0.25, 0.25, 0.25}
	w2, _ := tensorx.NewTensor(w2Data, tensorx.Shape{4, 1})

	mathMlp = tensorx.NewMLP(w1, w2)
}

func InitMLP(wal *rag.WAL) {
	w1Data, w2Data, err := wal.LoadWeights()
	if err == nil && len(w1Data) == 20 && len(w2Data) == 4 {
		w1, _ := tensorx.NewTensor(w1Data, tensorx.Shape{5, 4})
		w2, _ := tensorx.NewTensor(w2Data, tensorx.Shape{4, 1})
		mathMlp = tensorx.NewMLP(w1, w2)
	} else {
		wal.SaveWeights(mathMlp.W1.Data, mathMlp.W2.Data)
	}
}

func GetMathMLP() *tensorx.MLP {
	return mathMlp
}

// computeVectorizedHeuristics runs a fast single-pass $O(N)$ string analytical state machine.
func computeVectorizedHeuristics(code string) (float64, int, int, int, int) {
	if len(code) == 0 {
		return 0.0, 0, 0, 0, 0
	}
	var counts [256]int
	var connectors, cyclomatic, escapes int
	length := len(code)
	for i := 0; i < length; i++ {
		counts[code[i]]++
		switch code[i] {
		case 'f':
			dc, dy := scanByteF(code, i, length)
			connectors += dc
			cyclomatic += dy
		case 'g':
			connectors += scanByteG(code, i, length)
		case 'n':
			dc, de := scanByteN(code, i, length)
			connectors += dc
			escapes += de
		case '*':
			connectors++
		case 'i':
			cyclomatic += scanByteI(code, i, length)
		case 'c':
			cyclomatic += scanByteC(code, i, length)
		case '&':
			de, dy := scanByteAmpersand(code, i, length)
			escapes += de
			cyclomatic += dy
		case '|':
			cyclomatic += scanBytePipe(code, i, length)
		case 'm':
			escapes += scanByteM(code, i, length)
		case '@':
			escapes += scanByteAt(code, i, length)
		case 'a':
			escapes += scanByteA(code, i, length)
		}
	}
	return computeShannonEntropy(counts, length), length, connectors, cyclomatic, escapes
}

func scanByteF(code string, i, length int) (dConn, dCyc int) {
	if i+3 < length && code[i+1] == 'o' && code[i+2] == 'r' && code[i+3] == ' ' {
		return 1, 1
	}
	return 0, 0
}

func scanByteG(code string, i, length int) int {
	if i+2 < length && code[i+1] == 'o' && code[i+2] == ' ' {
		return 1
	}
	return 0
}

func scanByteN(code string, i, length int) (dConn, dEsc int) {
	if i+3 < length && code[i+1] == 'e' && code[i+2] == 'w' && code[i+3] == '(' {
		return 1, 1
	}
	return 0, 0
}

func scanByteI(code string, i, length int) int {
	if i+2 < length && code[i+1] == 'f' && code[i+2] == ' ' {
		return 1
	}
	return 0
}

func scanByteC(code string, i, length int) int {
	if i+4 < length && code[i+1] == 'a' && code[i+2] == 's' && code[i+3] == 'e' && code[i+4] == ' ' {
		return 1
	}
	return 0
}

func scanByteAmpersand(code string, i, length int) (dEsc, dCyc int) {
	dEsc = 1
	if i+1 < length && code[i+1] == '&' {
		dCyc = 1
	}
	return dEsc, dCyc
}

func scanBytePipe(code string, i, length int) int {
	if i+1 < length && code[i+1] == '|' {
		return 1
	}
	return 0
}

func scanByteM(code string, i, length int) int {
	if i+4 < length && code[i+1] == 'a' && code[i+2] == 'k' && code[i+3] == 'e' && code[i+4] == '(' {
		return 1
	}
	return 0
}

func scanByteAt(code string, i, length int) int {
	// [SRE Polyglot] TS-Ignore es Termodinámicamente Letal — penalty 15
	if i+9 < length && code[i+1] == 't' && code[i+2] == 's' && code[i+3] == '-' && code[i+4] == 'i' && code[i+5] == 'g' {
		return 15
	}
	return 0
}

func scanByteA(code string, i, length int) int {
	// [SRE Polyglot] Tipo "any" detectado — penalty 6
	if i+3 < length && code[i+1] == 'n' && code[i+2] == 'y' {
		c := code[i+3]
		if c == ' ' || c == ';' || c == ',' || c == ')' || c == '>' || c == ']' {
			return 6
		}
	}
	return 0
}

func computeShannonEntropy(counts [256]int, length int) float64 {
	entropy := 0.0
	fLength := float64(length)
	for _, count := range counts {
		if count > 0 {
			p := float64(count) / fLength
			entropy -= p * math.Log2(p)
		}
	}
	return entropy
}

func EvaluatePlan(ctx context.Context, cpu *tensorx.CPUDevice, code string) (score float32, metricsTable string, err error) {
	// Reemplazo Vectorial Single-Pass
	entropy, length, connectors, cyclomatic, escapes := computeVectorizedHeuristics(code)

	entropyScaled := float32(math.Min(entropy/8.0, 1.0))
	lengthScaled := float32(math.Min(float64(length)/1000.0, 1.0))
	connectorsScaled := float32(math.Min(float64(connectors)/10.0, 1.0))
	cycScaled := float32(math.Min(float64(cyclomatic)/20.0, 1.0))
	escScaled := float32(math.Min(float64(escapes)/5.0, 1.0))

	inputData := []float32{entropyScaled, connectorsScaled, lengthScaled, cycScaled, escScaled}
	input, _ := tensorx.NewTensor(inputData, tensorx.Shape{1, 5})

	score, err = mathMlp.Forward(ctx, cpu, input)

	var verdict string
	if err != nil {
		verdict = "VETADA"
		err = mathError("quantum bouncer forward pass failed: %v", err)
	} else if score < 0 {
		verdict = "VETADA"
		err = mathError("veto activated: MLP neural logit is negative (%f)", score)
	} else {
		verdict = "APROBADA"
	}

	metricsTable = fmt.Sprintf("\n| Métrica | Valor Crudo | Escala Min-Max | Impacto Neural |\n|---------|-------------|----------------|----------------|\n| Entropía| %f | %f | Positivo (+0.5)|\n| Longitud| %d | %f | Negativo (-0.1)|\n| Asintota| %d | %f | Activo (-0.5)  |\n| Ciclos  | %d | %f | Severo (-0.8)  |\n| Escapes | %d | %f | Crítico (-0.6) |\n**Veredicto:** %s | **Score Final:** %f\n",
		entropy, entropyScaled, length, lengthScaled, connectors, connectorsScaled, cyclomatic, cycScaled, escapes, escScaled, verdict, score)

	return score, metricsTable, err
}

func mathError(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}
