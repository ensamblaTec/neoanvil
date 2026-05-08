package dba

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"

	_ "modernc.org/sqlite" // Pure Go SQLite (CGO-Free)
)

type Analyzer struct {
	db *sql.DB
}

// NewAnalyzer crea una abstraccion protegida a la db local.
// Zero-Alloc connection pooler.
func NewAnalyzer(dbPath string) (*Analyzer, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("fallo abriendo sqlite: %v", err)
	}

	// SRE Throttling para prevenir ataques de File Descriptor exhaustion
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(5)

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("fallo ping db: %v", err)
	}

	log.Printf("[SRE-DBA] Motor Analitico Activo. Max Conns: %d", 5)
	return &Analyzer{db: db}, nil
}

// ApplySafeMigration inyecta una sentencia garantizando un rollback en caso de pánico SRE
func (a *Analyzer) ApplySafeMigration(ctx context.Context, safeQuery string) error {
	log.Println("[SRE-DBA] Ejecutando Single-Pass mutación atomica en BD")

	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("fallo arrancando transaccion SRE: %v", err)
	}

	defer func() {
		// Rollback ciego protege contra panicos
		_ = tx.Rollback()
	}()

	if _, err := tx.ExecContext(ctx, safeQuery); err != nil {
		return fmt.Errorf("fallo sintaxis query mutante: %v", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("fallo commit ACID: %v", err)
	}

	log.Println("[SRE-DBA] Mutacion SQL aplicada exitosamente")
	return nil
}

// QuerySchema executes a read-only schema query on an external database.
// maxOpenConns caps the connection pool; 0 defaults to 2 (safe for schema inspection).
func (a *Analyzer) QuerySchema(ctx context.Context, driver, dsn, query string, maxOpenConns int) (string, error) {
	if maxOpenConns <= 0 {
		maxOpenConns = 2
	}
	dbConn, err := sql.Open(driver, dsn)
	if err != nil {
		return "", fmt.Errorf("failed to open database connection: %w", err)
	}
	defer dbConn.Close()
	dbConn.SetMaxOpenConns(maxOpenConns)
	dbConn.SetMaxIdleConns(1)

	rows, err := dbConn.QueryContext(ctx, query)
	if err != nil {
		return "", fmt.Errorf("query execution failed: %w", err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return "", err
	}

	var results []string
	for rows.Next() {
		columns := make([]any, len(cols))
		columnPointers := make([]any, len(cols))
		for i := range columns {
			columnPointers[i] = &columns[i]
		}
		if err := rows.Scan(columnPointers...); err != nil {
			return "", err
		}
		var rowStr strings.Builder
		rowStr.WriteString("{")
		for i, colName := range cols {
			val := *(columnPointers[i].(*any))
			var valStr string
			switch v := val.(type) {
			case []byte:
				valStr = string(v)
			case nil:
				valStr = "NULL"
			default:
				valStr = fmt.Sprintf("%v", v)
			}
			rowStr.WriteString(fmt.Sprintf("%s: %s, ", colName, valStr))
		}
		rowStr.WriteString("}")
		results = append(results, rowStr.String())
	}
	return fmt.Sprintf("Columns: %v\nRows:\n%s", cols, fmt.Sprint(results)), nil
}
