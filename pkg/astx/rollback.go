package astx

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
)

func SaveRollback(workspace, filename string, content any) {
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(filename)))
	path := filepath.Join(workspace, ".neo", "rollback", hash+".bak")
	os.MkdirAll(filepath.Dir(path), 0755)
	
	switch v := content.(type) {
	case string:
		os.WriteFile(path, []byte(v), 0644)
	case []byte:
		os.WriteFile(path, v, 0644)
	}
}

func LoadRollback(workspace, filename string) (any, bool) {
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(filename)))
	path := filepath.Join(workspace, ".neo", "rollback", hash+".bak")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	return string(data), true
}

func DeleteRollback(workspace, filename string) {
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(filename)))
	path := filepath.Join(workspace, ".neo", "rollback", hash+".bak")
	os.Remove(path)
}