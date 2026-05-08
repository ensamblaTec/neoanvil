package astx

import (
	"go/parser"
	"go/token"
	"testing"
)

func TestCalculateCC(t *testing.T) {
	src := `package main

func simple() {
	println("hello")
}

func complex(a, b int) {
	if a > 0 && b > 0 {
		for i := 0; i < a; i++ {
			if i%2 == 0 || i%3 == 0 {
				println(i)
			}
		}
	} else if a < 0 {
		switch a {
		case -1:
			println(1)
		case -2:
			println(2)
		}
	}
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", []byte(src), 0)
	if err != nil {
		t.Fatal(err)
	}

	cc1 := CalculateCC(f.Decls[0])
	if cc1 != 1 {
		t.Errorf("Expected simple CC 1, got %d", cc1)
	}

	cc2 := CalculateCC(f.Decls[1])
	if cc2 != 9 {
		t.Errorf("Expected complex CC 9, got %d", cc2)
	}

	total, err := CalculateFileCC([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if total != 10 {
		t.Errorf("Expected total CC 10, got %d", total)
	}
}
