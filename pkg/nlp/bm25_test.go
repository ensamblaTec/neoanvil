package nlp

import "testing"

func TestBM25(t *testing.T) {
	texts := []string{
		"type User struct {\n ID string\n Password string\n}",          // Struct
		"func ValidateJWT(token string) bool { return token != \"\" }", // validation function
		"func GeneratePassword() {}",
	}

	bm := NewBM25(texts)

	scoreStruct := bm.Score("validate token", 0)
	scoreFunc := bm.Score("validate token", 1)

	if scoreFunc <= scoreStruct {
		t.Errorf("Expected function to have higher score than struct for 'validate token'. func=%f struct=%f", scoreFunc, scoreStruct)
	}

	if scoreFunc == 0 {
		t.Error("Expected function score > 0")
	}
}
